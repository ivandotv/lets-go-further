package testutil

// (The package doc lives in doc.go, which also carries the guide to the whole
// test suite. A package may only have one doc comment, so this file starts with
// a plain comment instead.)
//
// ── WHY THIS IS A PACKAGE AND NOT A _test.go FILE ────────────────────────────
//
// Helpers declared in a _test.go file are visible only to the package that
// declares them — the Go compiler doesn't export them, and you can't import
// them from elsewhere. Both internal/data and cmd/api need a test database, so
// this has to be an ordinary package to be shared.
//
// That means it imports "testing" from non-test code, which is normally a smell:
// it drags the testing package's flags and globals into anything that imports
// it. It's fine here because nothing on the production build path imports
// testutil, so the testing package is never linked into the api binary. (You can
// confirm that with `go list -deps ./cmd/api | grep testing`, which comes back
// empty.)

import (
	"database/sql"
	"path/filepath"
	"testing"

	"greenlight/internal/db"
)

// NewDB returns a fresh, fully-migrated SQLite database for a single test.
//
// ── WHY A TEMP FILE RATHER THAN :memory:? ────────────────────────────────────
//
// An in-memory SQLite database belongs to the CONNECTION that created it. Since
// database/sql manages a *pool* and is free to close idle connections and open
// new ones, an in-memory database can silently vanish mid-test. There's a
// `cache=shared` mode that works around it, but it comes with its own locking
// quirks.
//
// A file in t.TempDir() sidesteps all of that, and has a real advantage: the
// tests then exercise exactly the same code path as production, WAL journalling
// and all. Go removes the directory automatically when the test finishes, so
// there's nothing to clean up.
//
// Each test gets its OWN database, which means tests are fully isolated and can
// safely run in parallel — no shared state, no ordering dependencies, no
// truncating tables between cases.
//
// The parameter is testing.TB rather than *testing.T so that benchmarks can use
// it too. TB is the interface both *testing.T and *testing.B satisfy, and every
// method used below (Helper, TempDir, Fatalf, Cleanup) is on it.
func NewDB(t testing.TB) *sql.DB {
	// t.Helper() makes failures report against the calling test's line.
	t.Helper()

	// t.TempDir() creates a unique directory per test and registers its own
	// cleanup.
	dsn := filepath.Join(t.TempDir(), "test.db")

	database, err := db.OpenDB(db.Config{
		DSN:          dsn,
		MaxOpenConns: 2,
		MaxIdleConns: 2,
		MaxIdleTime:  0,
	})
	if err != nil {
		t.Fatalf("testutil: opening test database: %v", err)
	}

	// Run the embedded migrations. This is the payoff for embedding them:
	// tests get the real schema with no external CLI, no fixture .sql file to
	// keep in sync, and no chance of testing against a stale schema.
	if err := db.MigrateUp(database); err != nil {
		t.Fatalf("testutil: migrating test database: %v", err)
	}

	// t.Cleanup runs when the test (and all its subtests) finish, in LIFO
	// order. It's preferable to `defer` in a helper, because a deferred call
	// here would run when NewDB returns — long before the test is done.
	t.Cleanup(func() {
		database.Close()
	})

	return database
}
