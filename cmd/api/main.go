// Command api is the Greenlight JSON API server.
//
// The layout follows the convention from both of Alex Edwards' books:
//
//	cmd/api/       the executable, and everything HTTP-specific
//	internal/      packages that are private to this module
//	migrations/    the database schema, as versioned .sql files
//
// Anything under internal/ can only be imported by code inside this module.
// That's enforced by the Go toolchain itself, not by convention — it's a real
// compile error to import it from elsewhere. It makes internal/ a good home
// for code you don't want to commit to as a public API.
package main

import (
	"expvar"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"greenlight/internal/data"
	"greenlight/internal/db"
	"greenlight/internal/mailer"
)

// version is reported by the healthcheck endpoint and the -version flag.
//
// It's populated at build time from the version control information Go embeds
// automatically — see the vcsRevision function at the bottom of this file.
var version = vcsRevision()

// config holds every setting the application needs, all of it sourced from
// command-line flags.
//
// Keeping configuration in one struct (rather than reading globals or the
// environment from scattered places) means there's exactly one place to look
// to find out what's tunable, and tests can construct a config literal
// directly.
type config struct {
	port int
	env  string

	db struct {
		dsn          string
		maxOpenConns int
		maxIdleConns int
		maxIdleTime  time.Duration
	}

	// limiter configures the per-client rate limiter middleware.
	limiter struct {
		rps     float64
		burst   int
		enabled bool
	}

	smtp struct {
		host     string
		port     int
		username string
		password string
		sender   string
	}

	cors struct {
		trustedOrigins []string
	}
}

// Mailer is the interface the application needs for sending email.
//
// ── A DELIBERATE DEVIATION FROM THE BOOK ─────────────────────────────────────
//
// The book stores a concrete `mailer.Mailer` struct on the application. That's
// simpler, but it makes handlers untestable without a real SMTP server
// listening somewhere.
//
// By depending on a one-method interface instead, tests can inject a mock that
// just records what would have been sent (see cmd/api/testutils_test.go), and
// production injects the real SMTP-backed *mailer.Mailer. This is the standard
// Go idiom: define the interface where it is USED (the consumer), not where
// the implementation lives.
type Mailer interface {
	Send(recipient, templateFile string, data any) error
}

// application is the dependency-injection container.
//
// Every handler and middleware is defined as a method on *application, which
// is how they get access to the logger, the models, the config and so on
// without a single global variable. This is the central pattern of both books.
type application struct {
	config config
	logger *slog.Logger
	models data.Models
	mailer Mailer

	// wg tracks in-flight background goroutines (e.g. sending a welcome
	// email). Graceful shutdown waits on it, so the process doesn't exit while
	// a goroutine is halfway through sending mail.
	//
	// A sync.WaitGroup must not be copied after first use, which is one reason
	// application is always passed around as a POINTER.
	wg sync.WaitGroup
}

func main() {
	// main() does as little as possible: parse flags, then hand off. Everything
	// that can fail lives in run(), which returns an error rather than calling
	// os.Exit — that way deferred cleanup actually runs.
	if err := run(); err != nil {
		// The logger may not exist yet if setup failed early, so write to
		// stderr directly.
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config

	// ── Flags ────────────────────────────────────────────────────────────────
	//
	// Every flag has a sensible default so `go run ./cmd/api` works with no
	// arguments at all.

	flag.IntVar(&cfg.port, "port", 4000, "API server port")
	flag.StringVar(&cfg.env, "env", "development", "Environment (development|staging|production)")

	// The DSN is just a file path for SQLite. The connection options (WAL,
	// busy timeout, foreign keys) are added by internal/db.
	flag.StringVar(&cfg.db.dsn, "db-dsn", "greenlight.db", "SQLite database file path")

	// See the long comment in internal/db.OpenDB for why these defaults are so
	// much smaller than the book's Postgres values.
	flag.IntVar(&cfg.db.maxOpenConns, "db-max-open-conns", 4, "SQLite max open connections")
	flag.IntVar(&cfg.db.maxIdleConns, "db-max-idle-conns", 4, "SQLite max idle connections")
	flag.DurationVar(&cfg.db.maxIdleTime, "db-max-idle-time", 15*time.Minute, "SQLite max connection idle time")

	flag.Float64Var(&cfg.limiter.rps, "limiter-rps", 2, "Rate limiter maximum requests per second")
	flag.IntVar(&cfg.limiter.burst, "limiter-burst", 4, "Rate limiter maximum burst")
	flag.BoolVar(&cfg.limiter.enabled, "limiter-enabled", true, "Enable rate limiter")

	flag.StringVar(&cfg.smtp.host, "smtp-host", "", "SMTP host (empty = log emails instead of sending)")
	flag.IntVar(&cfg.smtp.port, "smtp-port", 25, "SMTP port")
	flag.StringVar(&cfg.smtp.username, "smtp-username", "", "SMTP username")
	flag.StringVar(&cfg.smtp.password, "smtp-password", "", "SMTP password")
	flag.StringVar(&cfg.smtp.sender, "smtp-sender", "Greenlight <no-reply@greenlight.example.com>", "SMTP sender")

	// flag.Func runs a callback when the flag is parsed, which lets us accept
	// a space-separated list and split it into a slice.
	flag.Func("cors-trusted-origins", "Space separated list of trusted CORS origins", func(val string) error {
		cfg.cors.trustedOrigins = strings.Fields(val)
		return nil
	})

	displayVersion := flag.Bool("version", false, "Display version and exit")

	flag.Parse()

	if *displayVersion {
		fmt.Printf("Version:\t%s\n", version)
		return nil
	}

	// ── Logger ───────────────────────────────────────────────────────────────
	//
	// log/slog is the standard library's structured logger (Go 1.21+). The
	// book predates it and hand-rolls a `jsonlog` package; slog does the same
	// job and is now the idiomatic choice, so we use it here.
	//
	// JSON output means log aggregators can parse and index the fields rather
	// than regex-matching free text.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// ── Database ─────────────────────────────────────────────────────────────

	database, err := db.OpenDB(db.Config{
		DSN:          cfg.db.dsn,
		MaxOpenConns: cfg.db.maxOpenConns,
		MaxIdleConns: cfg.db.maxIdleConns,
		MaxIdleTime:  cfg.db.maxIdleTime,
	})
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	// This defer is why run() exists as a separate function from main(): a
	// deferred call does NOT run if you call os.Exit.
	defer database.Close()

	// Apply any pending migrations on startup. Safe to run every time — it's a
	// no-op once the schema is current.
	if err := db.MigrateUp(database); err != nil {
		return err
	}

	logger.Info("database connection pool established", "dsn", cfg.db.dsn)

	// ── Metrics ──────────────────────────────────────────────────────────────
	//
	// expvar publishes these at GET /debug/vars as JSON. expvar.Publish takes
	// anything implementing expvar.Var (a String() string method);
	// expvar.Func adapts a plain func.
	//
	// These are evaluated lazily, at the moment /debug/vars is scraped.
	expvar.NewString("version").Set(version)

	expvar.Publish("goroutines", expvar.Func(func() any {
		return runtime.NumGoroutine()
	}))

	expvar.Publish("database", expvar.Func(func() any {
		// Pool statistics: open connections, how long requests waited for one,
		// etc. Very useful for spotting pool exhaustion.
		return database.Stats()
	}))

	expvar.Publish("timestamp", expvar.Func(func() any {
		return time.Now().Unix()
	}))

	// ── Application ──────────────────────────────────────────────────────────

	app := &application{
		config: cfg,
		logger: logger,
		models: data.NewModels(database),
		mailer: newMailer(cfg, logger),
	}

	return app.serve()
}

// newMailer returns a real SMTP mailer, or a logging stand-in if no SMTP host
// was configured.
//
// This is a quality-of-life addition rather than something from the book: it
// means you can clone this repo and register a user immediately, reading the
// activation token straight out of the application logs, with no Mailtrap
// account or SMTP server required.
func newMailer(cfg config, logger *slog.Logger) Mailer {
	if cfg.smtp.host == "" {
		logger.Info("no -smtp-host configured; emails will be logged instead of sent")
		return logMailer{logger: logger}
	}

	return mailer.New(cfg.smtp.host, cfg.smtp.port, cfg.smtp.username, cfg.smtp.password, cfg.smtp.sender)
}

// logMailer implements Mailer by writing to the log instead of sending mail.
type logMailer struct {
	logger *slog.Logger
}

// Send logs the email that would have been sent, including the template data —
// which is what makes the activation token visible during local development.
func (m logMailer) Send(recipient, templateFile string, data any) error {
	m.logger.Info("email (not sent: no SMTP host configured)",
		"recipient", recipient,
		"template", templateFile,
		"data", fmt.Sprintf("%+v", data),
	)

	return nil
}

// background runs fn in a new goroutine, tracked by the application's
// WaitGroup, with panic recovery.
//
// ── WHY THIS HELPER EXISTS ───────────────────────────────────────────────────
//
// Sending a welcome email takes a second or two over SMTP. Doing it inline
// would make POST /v1/users feel sluggish for no benefit to the client, who
// doesn't care whether the mail has left yet.
//
// But firing off a bare `go func() {...}` has two serious problems:
//
//  1. A panic in a goroutine takes down the ENTIRE program. The recoverPanic
//     middleware can't help — it only covers the request's own goroutine. So
//     every background goroutine needs its own recover.
//
//  2. Graceful shutdown would kill the goroutine mid-send. The WaitGroup lets
//     serve() wait for outstanding work before exiting.
//
// Add(1) must happen HERE, in the calling goroutine, not inside the new one —
// otherwise Wait() could run before Add() and return too early.
func (app *application) background(fn func()) {
	app.wg.Add(1)

	go func() {
		// Runs when the goroutine finishes, whether normally or via panic.
		defer app.wg.Done()

		defer func() {
			if r := recover(); r != nil {
				// %v because a recovered value is `any` and might not be an
				// error at all.
				app.logger.Error("recovered panic in background goroutine", "error", fmt.Sprintf("%v", r))
			}
		}()

		fn()
	}()
}

// vcsRevision reads the version control revision that Go embeds into the
// binary at build time.
//
// Since Go 1.18 the toolchain records the VCS commit automatically, so we get
// a real version string with zero build-time flags — no need for the book's
// `-ldflags="-X main.version=..."` dance. If the build has no VCS info
// (`go run` from a directory that isn't a repo, for example) we fall back to a
// placeholder.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unavailable"
	}

	var revision, modified string

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	if revision == "" {
		return "unavailable"
	}

	// Short hash is plenty for identification.
	if len(revision) > 12 {
		revision = revision[:12]
	}

	// A "-dirty" suffix warns that the build included uncommitted changes, so
	// the commit hash alone doesn't fully describe what's running.
	if modified == "true" {
		return revision + "-dirty"
	}

	return revision
}

// ensure the concrete mailer satisfies the interface at compile time.
// This is a zero-cost assertion: if *mailer.Mailer ever stops implementing
// Mailer, the build breaks here with a clear message rather than somewhere
// confusing.
var _ Mailer = (*mailer.Mailer)(nil)
