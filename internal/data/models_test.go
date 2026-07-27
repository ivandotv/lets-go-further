package data

import (
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"greenlight/internal/testutil"
)

// TestMain runs once for the whole package, wrapping every test in it.
//
// We use it to drop the bcrypt work factor to the minimum. At the production
// cost of 12, each password hash takes ~250ms on purpose — and this suite
// creates a few dozen users, which would put most of the runtime into
// deliberately-wasted CPU. At MinCost the same suite runs in well under a
// second.
//
// This is safe because the cost factor is stored inside each hash, so
// verification still works regardless of what cost was used to create it.
// We're testing our own logic here, not bcrypt's.
//
// The os.Exit is required: m.Run() returns the exit code rather than exiting
// itself, so TestMain must pass it on.
func TestMain(m *testing.M) {
	BcryptCost = bcrypt.MinCost

	os.Exit(m.Run())
}

// ─────────────────────────────────────────────────────────────────────────────
// INTEGRATION TESTS FOR THE MODEL LAYER
// ─────────────────────────────────────────────────────────────────────────────
//
// Everything in the *_test.go files that calls newTestModels talks to a REAL
// SQLite database with the real schema. That's the point: these tests verify
// our SQL, which is precisely the part a mock would not check.
//
// Because SQLite is an embedded library and the database is just a temp file,
// there's no server to start and no fixtures to load — so these run in
// milliseconds and there's no reason to avoid them. (With Postgres you'd need
// Docker or a live server, which is why the book's model layer is much less
// tested than its handlers.)

// newTestModels returns a Models value backed by a fresh, migrated database.
//
// It skips the test entirely under `go test -short`, following the convention
// from the first book: `-short` runs only the fast, dependency-free unit tests.
func newTestModels(t *testing.T) Models {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping database integration test with -short")
	}

	return NewModels(testutil.NewDB(t))
}
