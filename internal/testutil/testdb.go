// Package testutil provides shared helpers for tests.
//
// It lives in its own package (rather than in a _test.go file) because both
// internal/data and cmd/api need it, and helpers in a _test.go file are only
// visible to the package they're declared in.
//
// This package imports "testing", which is normally something to avoid in
// non-test code. It's fine here because nothing on the production build path
// imports testutil, so the testing package never gets linked into the api
// binary.
package testutil

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
func NewDB(t *testing.T) *sql.DB {
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
