// Command seed populates the database with a demo user and some sample movies,
// then prints a ready-to-use authentication token.
//
// This isn't from either book — in the book you'd grant permissions by hand
// with psql. It exists so that a fresh clone of this repo can go from `git
// clone` to a working authenticated request in one command, which makes the
// project much easier to explore.
//
// Usage:
//
//	go run ./cmd/seed -db-dsn=greenlight.db
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"greenlight/internal/data"
	"greenlight/internal/db"
	"greenlight/internal/validator"
)

// config holds everything the seeder needs, so that run() takes plain values
// rather than reading global flag state.
//
// Splitting it out this way is what makes run() testable: the global
// flag.CommandLine set can only be parsed once per process, so a run() that
// called flag.Parse() itself could never be exercised twice from a test binary.
type config struct {
	dsn        string
	email      string
	password   string
	fixedToken string
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		// flag itself has already printed the usage message.
		os.Exit(1)
	}

	if err := run(cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		os.Exit(1)
	}
}

// parseFlags builds a config from command-line arguments.
//
// It uses its own FlagSet rather than the package-level one, with
// ContinueOnError so a bad argument returns instead of calling os.Exit — which
// would take a test binary down with it.
func parseFlags(args []string) (config, error) {
	var cfg config

	fs := flag.NewFlagSet("seed", flag.ContinueOnError)

	fs.StringVar(&cfg.dsn, "db-dsn", "greenlight.db", "SQLite database file path")
	fs.StringVar(&cfg.email, "email", "demo@example.com", "Email address for the demo user")
	fs.StringVar(&cfg.password, "password", "pa55word1234", "Password for the demo user")

	// Fixed rather than randomly generated, so re-running this command — or
	// just re-reading the README — always gets you the same value. That means
	// tools like the Bruno collection can hardcode it and skip the
	// copy-a-fresh-token-in dance entirely.
	//
	// This is safe ONLY because it's confined to this local dev-seeding tool:
	// the real API (cmd/api) never issues or accepts a token by any means
	// other than TokenModel.New's crypto/rand generation. Nobody else has your
	// SQLite file, so a known plaintext is no more sensitive than a well-known
	// local "postgres/postgres" dev credential.
	fs.StringVar(&cfg.fixedToken, "token", "GREENLIGHT0000000000000000",
		"Fixed plaintext authentication token to seed for the demo user (must be 26 characters)")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	return cfg, nil
}

// run does the seeding, writing its progress to out.
//
// Taking an io.Writer rather than printing to stdout directly means a test can
// assert on what the command reports, which is most of its user-visible
// behaviour.
func run(cfg config, out io.Writer) error {
	v := validator.New()
	if data.ValidateTokenPlaintext(v, cfg.fixedToken); !v.Valid() {
		return fmt.Errorf("invalid -token: %s", v.Errors["token"])
	}

	database, err := db.OpenDB(db.Config{
		DSN:          cfg.dsn,
		MaxOpenConns: 2,
		MaxIdleConns: 2,
		MaxIdleTime:  time.Minute,
	})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	// Make sure the schema exists — seeding a brand-new database should just
	// work without running the API server first.
	if err := db.MigrateUp(database); err != nil {
		return err
	}

	models := data.NewModels(database)

	// ── The demo user ────────────────────────────────────────────────────────
	//
	// Created already activated, and granted BOTH permissions, so it can
	// exercise every endpoint. (A user registering through the API normally
	// starts deactivated with only movies:read.)
	user := &data.User{
		Name:      "Demo User",
		Email:     cfg.email,
		Activated: true,
	}

	if err := user.Password.Set(cfg.password); err != nil {
		return err
	}

	err = models.Users.Insert(user)
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(out, "created user %s\n", user.Email)

	case errors.Is(err, data.ErrDuplicateEmail):
		// Make the command re-runnable: if the user already exists, look them
		// up and carry on rather than failing.
		_, _ = fmt.Fprintf(out, "user %s already exists, reusing it\n", cfg.email)

		user, err = models.Users.GetByEmail(cfg.email)
		if err != nil {
			return err
		}

	default:
		return err
	}

	// AddForUser uses INSERT OR IGNORE, so re-granting is a no-op.
	err = models.Permissions.AddForUser(user.ID,
		data.PermissionMoviesRead, data.PermissionMoviesWrite)
	if err != nil {
		return err
	}

	// ── Sample movies ────────────────────────────────────────────────────────
	movies := []struct {
		title   string
		year    int32
		runtime data.Runtime
		genres  []string
	}{
		{"Casablanca", 1942, 102, []string{"drama", "romance", "war"}},
		{"The Breakfast Club", 1985, 96, []string{"drama", "comedy"}},
		{"Deadpool", 2016, 108, []string{"action", "comedy"}},
		{"Moana", 2016, 107, []string{"animation", "adventure"}},
		{"Black Panther", 2018, 134, []string{"action", "adventure", "sci-fi"}},
		{"Everything Everywhere All at Once", 2022, 139, []string{"action", "comedy", "sci-fi"}},
	}

	// Only insert the sample movies into an empty table, so re-running the
	// command doesn't pile up duplicates.
	filters := data.Filters{Page: 1, PageSize: 1, Sort: "id", SortSafelist: []string{"id"}}

	existing, _, err := models.Movies.GetAll("", nil, filters)
	if err != nil {
		return err
	}

	if len(existing) > 0 {
		_, _ = fmt.Fprintln(out, "movies table is not empty, skipping sample movies")
	} else {
		for _, m := range movies {
			movie := &data.Movie{
				Title:   m.title,
				Year:    m.year,
				Runtime: m.runtime,
				Genres:  data.Genres(m.genres),
			}

			if err := models.Movies.Insert(movie); err != nil {
				return err
			}
		}

		_, _ = fmt.Fprintf(out, "inserted %d sample movies\n", len(movies))
	}

	// ── A fixed authentication token to use straight away ────────────────────
	//
	// Burn any tokens from a previous seed run first, so re-running this
	// command doesn't leave old rows (fixed or, from before this existed,
	// randomly generated) sitting in the table alongside the new one.
	if err := models.Tokens.DeleteAllForUser(data.ScopeAuthentication, user.ID); err != nil {
		return err
	}

	hash := sha256.Sum256([]byte(cfg.fixedToken))

	token := &data.Token{
		Plaintext: cfg.fixedToken,
		Hash:      hash[:],
		UserID:    user.ID,
		// Long enough that it never practically expires in local dev. There's
		// no security cost to a long TTL here — see the comment on the -token
		// flag above for why this token being fixed and known is fine at all.
		Expiry: time.Now().AddDate(100, 0, 0),
		Scope:  data.ScopeAuthentication,
	}

	if err := models.Tokens.Insert(token); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, `
Demo user ready.

  email:    %s
  password: %s

Authentication token (fixed — same every time, doesn't expire):

  %s

Already set as the default in bruno/environments/Local.bru — no copying
required. Or try it directly:

  curl -H "Authorization: Bearer %s" localhost:4000/v1/movies

`, user.Email, cfg.password, token.Plaintext, token.Plaintext)

	return nil
}
