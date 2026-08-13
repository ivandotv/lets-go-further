// Package db handles opening the SQLite database and running migrations.
//
// In the book this logic lives in cmd/api/main.go and is only a few lines,
// because Postgres does the heavy lifting. SQLite needs a bit more care at
// connection time — mostly around PRAGMAs and write concurrency — so it gets
// its own package here, with the reasoning written down.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// Import the pure-Go SQLite driver for its side effects only. The blank
	// identifier says "I only want this package's init() to run" — that init
	// registers the driver under the name "sqlite" with database/sql.
	//
	// WHY modernc.org/sqlite RATHER THAN github.com/mattn/go-sqlite3?
	// mattn's driver is a cgo wrapper around the real C library. It's fast and
	// battle-tested, but needs a C toolchain, breaks `CGO_ENABLED=0` static
	// builds, and makes cross-compiling painful. modernc.org/sqlite is SQLite
	// *transpiled to Go*, so it's pure Go: no C compiler, trivial
	// cross-compilation, and `go test` just works everywhere.
	_ "modernc.org/sqlite"

	"greenlight/migrations"
)

// Config holds the tunables for the connection pool.
//
// These mirror the flags in the book (`-db-max-open-conns` etc.) but the
// sensible *values* are quite different for SQLite — see OpenDB.
type Config struct {
	// DSN is the path to the database file, e.g. "greenlight.db".
	// The special value ":memory:" gets special handling (see below).
	DSN string

	MaxOpenConns int
	MaxIdleConns int
	MaxIdleTime  time.Duration
}

// buildDSN turns a plain file path into a full SQLite connection string with
// the PRAGMAs we need.
//
// PRAGMAs are per-connection settings, and database/sql maintains a *pool* of
// connections that it opens and closes as it sees fit. So running
// `PRAGMA foreign_keys = ON` once after opening would only configure whichever
// connection happened to serve that statement. Putting the pragmas in the DSN
// makes the driver apply them to *every* connection it opens. This is the
// single most common SQLite-in-Go mistake, and it fails silently.
//
// The pragmas we set, and why:
//
//	foreign_keys(1)      SQLite ships with FK enforcement OFF for backwards
//	                     compatibility. Without this, ON DELETE CASCADE and
//	                     every REFERENCES clause is decorative.
//
//	journal_mode(WAL)    Write-Ahead Logging. In the default rollback-journal
//	                     mode a writer blocks all readers. Under WAL, readers
//	                     and a single writer proceed concurrently — essential
//	                     for a web server. WAL is persistent: it's a property
//	                     of the database file, not the connection, but it's
//	                     harmless to request every time.
//
//	busy_timeout(5000)   SQLite permits only ONE writer at a time across the
//	                     whole database. Without this, a second concurrent
//	                     write fails instantly with SQLITE_BUSY. With it, the
//	                     driver waits up to 5s for the lock instead. This turns
//	                     a hard error into a small latency blip.
//
//	synchronous(NORMAL)  With WAL, NORMAL is the recommended setting: durable
//	                     against application crashes, and only at risk from an
//	                     OS/power failure. FULL fsyncs on every commit and is
//	                     dramatically slower.
func buildDSN(path string) string {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "foreign_keys(1)")
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "synchronous(NORMAL)")

	return fmt.Sprintf("file:%s?%s", path, pragmas.Encode())
}

// OpenDB creates a connection pool, verifies it, and returns it.
//
// The caller is responsible for calling Close() on the returned *sql.DB —
// conventionally with `defer db.Close()` in main().
func OpenDB(cfg Config) (*sql.DB, error) {
	// sql.Open does NOT actually connect to anything. It just validates the
	// arguments and sets up the pool lazily. That's why we Ping below.
	database, err := sql.Open("sqlite", buildDSN(cfg.DSN))
	if err != nil {
		return nil, err
	}

	// ── Pool sizing ──────────────────────────────────────────────────────────
	//
	// This is where SQLite differs sharply from Postgres. The book sets
	// MaxOpenConns to 25 because Postgres genuinely handles 25 concurrent
	// writers. SQLite serialises ALL writes behind a single database-level
	// lock, so extra connections buy you nothing for writes — they only help
	// concurrent *reads* (which WAL does allow in parallel).
	//
	// So: a modest pool is right. Too large and you just have more goroutines
	// queueing on busy_timeout; too small and reads serialise unnecessarily.
	//
	// (The scrupulously correct setup is TWO pools — a read pool with N
	// connections and a write pool pinned to exactly 1, which eliminates
	// SQLITE_BUSY entirely. It's a great pattern, but it means threading two
	// handles through every model method, so this project keeps the single
	// pool + busy_timeout approach and mentions the alternative in the README.)
	database.SetMaxOpenConns(cfg.MaxOpenConns)
	database.SetMaxIdleConns(cfg.MaxIdleConns)
	database.SetConnMaxIdleTime(cfg.MaxIdleTime)

	// Now actually establish a connection, with a 5-second deadline so a
	// misconfigured path fails fast at startup rather than on the first
	// request.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := database.PingContext(ctx); err != nil {
		// Don't leak the pool if the ping failed.
		_ = database.Close()
		return nil, err
	}

	return database, nil
}

// MigrateUp applies any migrations that haven't run yet.
//
// It's safe to call on every startup: golang-migrate records the current
// version in a `schema_migrations` table and applies only what's missing,
// returning migrate.ErrNoChange when everything is already up to date.
func MigrateUp(database *sql.DB) error {
	// iofs adapts our embedded filesystem into a migration *source*. The "."
	// is the directory within the FS — our .sql files sit at its root.
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("db: loading migrations: %w", err)
	}
	defer func() { _ = source.Close() }()

	// WithInstance wraps our existing *sql.DB as a migration *target*, so
	// migrations run over the same pool (and therefore the same PRAGMAs) as
	// the rest of the application.
	driver, err := migratesqlite.WithInstance(database, &migratesqlite.Config{})
	if err != nil {
		return fmt.Errorf("db: creating migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("db: creating migrator: %w", err)
	}

	// NOTE: we deliberately do NOT call m.Close() here. Because we handed
	// migrate an existing *sql.DB, Close() would close the connection pool out
	// from under the application. We close the source ourselves (deferred
	// above) and let the caller own the pool's lifetime.

	err = m.Up()
	// ErrNoChange just means "already at the latest version", which is the
	// normal case on every restart after the first. errors.Is unwraps any
	// wrapping, so it's the right comparison rather than `err == `.
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: applying migrations: %w", err)
	}

	return nil
}
