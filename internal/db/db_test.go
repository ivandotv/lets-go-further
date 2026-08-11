package db

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strings"

	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"

	"greenlight/internal/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// WHY THIS FILE EXISTS
// ─────────────────────────────────────────────────────────────────────────────
//
// Every other package's tests reach the database through internal/testutil, so
// this package is exercised constantly — but only INDIRECTLY, and only in the
// one configuration testutil happens to use. That leaves the riskiest thing
// here unguarded.
//
// The risk is buildDSN. Its doc comment calls the "PRAGMA as a one-off
// statement" mistake "the single most common SQLite-in-Go mistake, and it fails
// silently" — and silence is the problem. Drop `foreign_keys(1)` and ON DELETE
// CASCADE quietly stops working. Drop `journal_mode(WAL)` and reads start
// blocking behind writes. Drop `busy_timeout(5000)` and concurrent writes fail
// with SQLITE_BUSY instead of waiting. None of those produce a compile error,
// and only the first is caught elsewhere (by TestTokenModel_CascadeDeleteOnUser
// over in internal/data).
//
// These tests are the tripwire for the other three.

// TestBuildDSN pins the connection string's shape and every pragma in it.
func TestBuildDSN(t *testing.T) {
	dsn := buildDSN("greenlight.db")

	// The "file:" prefix is what tells SQLite to interpret the rest as a URI
	// rather than a bare filename — without it the query string would be read
	// as part of the FILENAME, and every pragma would be silently ignored.
	if !strings.HasPrefix(dsn, "file:greenlight.db?") {
		t.Fatalf("got %q; want a file: URI for greenlight.db", dsn)
	}

	u, err := url.Parse(dsn)
	assert.NilError(t, err)

	// The pragmas arrive as repeated _pragma parameters. Sort before comparing
	// so this test pins WHICH pragmas are set without also freezing the order
	// they happen to be added in — that ordering carries no meaning to SQLite.
	got := u.Query()["_pragma"]
	want := []string{
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
	}

	assert.DeepEqual(t, got, want, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	}))
}

// TestBuildDSNEscapesPaths checks that a path needing escaping survives.
//
// t.TempDir() on macOS returns paths under /var/folders/... with characters
// that must be percent-encoded. If buildDSN concatenated strings naively, the
// tests in every other package would break in confusing ways on some machines
// and not others.
func TestBuildDSNEscapesPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "my database.db")

	u, err := url.Parse(buildDSN(path))
	assert.NilError(t, err)

	// Opaque holds everything between "file:" and "?" for a non-hierarchical
	// URI; Path holds it for one starting with "/". Temp dirs are absolute, so
	// it lands in Path.
	if u.Path != path {
		t.Errorf("got path %q; want %q", u.Path, path)
	}
}

// TestPragmasApplyToEveryConnection is the test that would actually catch the
// mistake buildDSN's comment warns about.
//
// A `PRAGMA foreign_keys = ON` statement run once after opening would pass a
// naive test: the very next query probably reuses that same connection. This
// test defeats that by holding several connections open SIMULTANEOUSLY —
// database/sql cannot hand back a connection that's still checked out, so each
// one is genuinely distinct — and asserting the pragmas on all of them.
func TestPragmasApplyToEveryConnection(t *testing.T) {
	const conns = 4

	database := openTestDB(t, conns)

	ctx := context.Background()

	// ── Forcing distinct connections ─────────────────────────────────────────
	//
	// database/sql hands out connections from a pool, and normally you never see
	// them — you call db.Query and it picks whichever is free. db.Conn(ctx) is
	// the exception: it checks out one connection and RESERVES it until you
	// Close() it.
	//
	// So by checking out four and holding them all, we're guaranteed four
	// genuinely different connections. That's what defeats a naive
	// implementation: a `PRAGMA foreign_keys = ON` run once at startup would
	// configure exactly one connection, and any test that queried through the
	// pool would probably get that same one back and pass.
	//
	// Note this needs MaxOpenConns >= conns, or db.Conn would block waiting for
	// a connection that's never coming — hence openTestDB(t, conns) above.
	held := make([]*sql.Conn, 0, conns)

	// Cleanup rather than defer: these must stay checked out for the whole test,
	// and t.Cleanup runs at the very end. (A defer would also work here since
	// this is the test function itself, but Cleanup is the habit to build — in a
	// helper, defer fires far too early.)
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})

	for i := range conns {
		conn, err := database.Conn(ctx)
		if err != nil {
			t.Fatalf("checking out connection %d: %v", i, err)
		}
		held = append(held, conn)
	}

	// PRAGMA returns the current value when called without an argument.
	// synchronous(NORMAL) reports as the integer 1.
	checks := []struct {
		pragma string
		want   string
	}{
		{"foreign_keys", "1"},
		{"journal_mode", "wal"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"},
	}

	for i, conn := range held {
		for _, c := range checks {
			var got string
			err := conn.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got)
			if err != nil {
				t.Fatalf("connection %d: reading PRAGMA %s: %v", i, c.pragma, err)
			}

			if !strings.EqualFold(got, c.want) {
				t.Errorf("connection %d: PRAGMA %s = %q; want %q",
					i, c.pragma, got, c.want)
			}
		}
	}
}

// TestForeignKeysAreEnforced proves the foreign_keys pragma has teeth, rather
// than just being reported as on.
func TestForeignKeysAreEnforced(t *testing.T) {
	database := openTestDB(t, 2)

	assert.NilError(t, MigrateUp(database))

	// user_id 99999 doesn't exist, and tokens.user_id REFERENCES users(id).
	_, err := database.Exec(
		`INSERT INTO tokens (hash, user_id, expiry, scope) VALUES (?, ?, ?, ?)`,
		[]byte("0123456789abcdef0123456789abcdef"), 99999, "2099-01-01T00:00:00Z", "authentication",
	)
	if err == nil {
		t.Fatal("inserting a token for a non-existent user succeeded; foreign keys are not being enforced")
	}

	assert.StringContains(t, strings.ToUpper(err.Error()), "FOREIGN KEY")
}

// TestOpenDBAppliesPoolSettings checks the Config fields reach the pool.
func TestOpenDBAppliesPoolSettings(t *testing.T) {
	database, err := OpenDB(Config{
		DSN:          filepath.Join(t.TempDir(), "test.db"),
		MaxOpenConns: 7,
		MaxIdleConns: 3,
		MaxIdleTime:  0,
	})
	assert.NilError(t, err)
	t.Cleanup(func() { database.Close() })

	assert.Equal(t, database.Stats().MaxOpenConnections, 7)
}

// TestOpenDBFailsOnUnusablePath checks that a bad path is reported at STARTUP
// rather than on the first request.
//
// This is the whole reason OpenDB pings: sql.Open only validates arguments and
// sets up the pool lazily, so without the ping a misconfigured -db-dsn would
// look like a healthy boot followed by mysteriously failing requests.
func TestOpenDBFailsOnUnusablePath(t *testing.T) {
	// A directory that doesn't exist — SQLite can create a file, but not the
	// directory holding it.
	dsn := filepath.Join(t.TempDir(), "no-such-dir", "test.db")

	database, err := OpenDB(Config{DSN: dsn, MaxOpenConns: 1, MaxIdleConns: 1})
	if err == nil {
		database.Close()
		t.Fatal("got nil error opening a database in a non-existent directory; want failure")
	}

	// OpenDB must also not hand back a half-open pool on the error path.
	if database != nil {
		t.Error("got a non-nil *sql.DB alongside an error; the pool should have been closed and dropped")
	}
}

// TestMigrateUpIsIdempotent covers the migrate.ErrNoChange branch — the normal
// case on every restart after the first.
func TestMigrateUpIsIdempotent(t *testing.T) {
	database := openTestDB(t, 2)

	assert.NilError(t, MigrateUp(database))

	// The second call must be a no-op, not an error. cmd/api calls MigrateUp on
	// every boot, so if this regressed the server would refuse to start on its
	// second run.
	assert.NilError(t, MigrateUp(database))

	// And a third, for good measure.
	assert.NilError(t, MigrateUp(database))
}

// TestMigrateUpLeavesThePoolOpen guards the deliberate "we do NOT call
// m.Close()" decision documented in MigrateUp.
//
// golang-migrate's Close() would shut down the *sql.DB we handed it — which is
// the application's own pool. If someone "tidies up" by adding a defer m.Close(),
// every query after startup would fail with "sql: database is closed", and this
// test is what says so.
func TestMigrateUpLeavesThePoolOpen(t *testing.T) {
	database := openTestDB(t, 2)

	assert.NilError(t, MigrateUp(database))

	var count int
	err := database.QueryRow(`SELECT count(*) FROM movies`).Scan(&count)
	assert.NilError(t, err)
	assert.Equal(t, count, 0)
}

// TestMigrateUpCreatesTheExpectedSchema is a light smoke test that the embedded
// .sql files actually ran, rather than the migrator silently finding no source.
func TestMigrateUpCreatesTheExpectedSchema(t *testing.T) {
	database := openTestDB(t, 2)

	assert.NilError(t, MigrateUp(database))

	for _, table := range []string{"movies", "users", "tokens", "permissions", "users_permissions"} {
		var name string
		err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after MigrateUp: %v", table, err)
		}
	}
}

// TestMigrateUpAcrossRestarts covers the real-world boot path: the server runs,
// exits, and starts again against the database file it left behind.
//
// TestMigrateUpIsIdempotent above calls MigrateUp twice on one pool; this goes
// further by closing the pool in between, so the second migration genuinely
// re-reads schema_migrations from disk rather than from anything cached.
//
// ── A KNOWN LIMITATION, DELIBERATELY NOT TESTED HERE ─────────────────────────
//
// Migrating CONCURRENTLY from two processes against the same file does NOT
// work: golang-migrate marks schema_migrations dirty before applying a
// migration and clears it after, and its SQLite driver has no advisory lock, so
// two racing boots leave "Dirty database version 1. Fix and force version." and
// need a manual `migrate force` to recover.
//
// That's a property of golang-migrate's SQLite driver, not of this code, and
// it only bites if you start two servers against one file at the same moment.
// It's written down in README.md rather than guarded by a test, because a test
// asserting that something is broken tends to get "fixed" by deleting it.
func TestMigrateUpAcrossRestarts(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")

	for boot := range 3 {
		database, err := OpenDB(Config{DSN: dsn, MaxOpenConns: 2, MaxIdleConns: 2})
		if err != nil {
			t.Fatalf("boot %d: opening: %v", boot, err)
		}

		if err := MigrateUp(database); err != nil {
			t.Fatalf("boot %d: migrating: %v", boot, err)
		}

		// Data written on one boot must still be there on the next — proof the
		// migrations aren't being re-applied destructively.
		if boot == 0 {
			_, err = database.Exec(
				`INSERT INTO movies (title, year, runtime, genres) VALUES (?, ?, ?, ?)`,
				"Persistent", 1994, 142, `["drama"]`,
			)
			assert.NilError(t, err)
		}

		var count int
		err = database.QueryRow(`SELECT count(*) FROM movies`).Scan(&count)
		assert.NilError(t, err)
		assert.Equal(t, count, 1)

		database.Close()
	}
}

// openTestDB opens a fresh database WITHOUT migrating it, so tests here can
// exercise MigrateUp themselves.
//
// (internal/testutil.NewDB does migrate, which is what every other package
// wants — but it would make the migration tests below circular.)
func openTestDB(t *testing.T, maxOpen int) *sql.DB {
	t.Helper()

	database, err := OpenDB(Config{
		DSN:          filepath.Join(t.TempDir(), "test.db"),
		MaxOpenConns: maxOpen,
		MaxIdleConns: maxOpen,
		MaxIdleTime:  0,
	})
	assert.NilError(t, err)

	t.Cleanup(func() { database.Close() })

	return database
}
