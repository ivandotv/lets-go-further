package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"greenlight/internal/assert"
	"greenlight/internal/data"
	"greenlight/internal/db"
)

// TestMain drops the bcrypt cost, for the same reason as the equivalents in
// internal/data and cmd/api: the seeder hashes a password, and at the
// production cost of 12 that's ~250ms of deliberately-wasted CPU per test.
func TestMain(m *testing.M) {
	data.BcryptCost = bcrypt.MinCost

	os.Exit(m.Run())
}

// ─────────────────────────────────────────────────────────────────────────────
// WHY TEST A DEV TOOL?
// ─────────────────────────────────────────────────────────────────────────────
//
// cmd/seed isn't production code, so it's tempting to leave it alone. But it's
// the first thing anyone runs after cloning this repo, and it's the only thing
// standing between `git clone` and a working authenticated request. If it
// breaks, the project looks broken.
//
// It's also the one place that writes a token row by hand rather than going
// through TokenModel.New, which means it duplicates the hashing logic — and
// duplicated logic is exactly what drifts.

// TestRunSeedsAFreshDatabase is the main path: an empty directory in, a fully
// usable database out.
func TestRunSeedsAFreshDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database-backed test with -short")
	}

	cfg := testConfig(t)

	var out bytes.Buffer

	assert.NilError(t, run(cfg, &out))

	// What it told the user.
	output := out.String()
	assert.StringContains(t, output, "created user demo@example.com")
	assert.StringContains(t, output, "inserted 6 sample movies")
	assert.StringContains(t, output, cfg.fixedToken)

	// What it actually wrote.
	models := openSeeded(t, cfg)

	user, err := models.Users.GetByEmail(cfg.email)
	assert.NilError(t, err)

	assert.Equal(t, user.Name, "Demo User")
	// The demo user is created ALREADY ACTIVATED, unlike one registering
	// through the API — that's the whole point of the seeder.
	assert.Equal(t, user.Activated, true)

	matches, err := user.Password.Matches(cfg.password)
	assert.NilError(t, err)
	assert.Equal(t, matches, true)

	// Both permissions, so the demo user can exercise every endpoint.
	permissions, err := models.Permissions.GetAllForUser(user.ID)
	assert.NilError(t, err)
	assert.Equal(t, permissions.Include(data.PermissionMoviesRead), true)
	assert.Equal(t, permissions.Include(data.PermissionMoviesWrite), true)

	movies, _, err := models.Movies.GetAll("", nil, data.Filters{
		Page: 1, PageSize: 100, Sort: "id", SortSafelist: []string{"id"},
	})
	assert.NilError(t, err)
	assert.Equal(t, len(movies), 6)
}

// TestRunSeedsAUsableToken is the assertion that matters most.
//
// The seeder inserts the token row itself rather than calling TokenModel.New,
// because New generates a random plaintext and the whole point here is a fixed
// one. That means it re-implements the SHA-256 hashing — so if the hashing in
// internal/data ever changed, the seeded token would silently stop
// authenticating and the README's copy-paste curl command would just 401.
//
// Looking the token up through the real UserModel.GetForToken is what catches
// that: it's the same code path the authenticate middleware uses.
func TestRunSeedsAUsableToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database-backed test with -short")
	}

	cfg := testConfig(t)

	assert.NilError(t, run(cfg, &bytes.Buffer{}))

	models := openSeeded(t, cfg)

	user, err := models.Users.GetForToken(data.ScopeAuthentication, cfg.fixedToken)
	assert.NilError(t, err)

	assert.Equal(t, user.Email, cfg.email)

	// And the stored hash really is SHA-256 of the plaintext — the duplicated
	// bit of logic this test exists to pin down.
	var stored []byte
	err = openDB(t, cfg).QueryRow(
		`SELECT hash FROM tokens WHERE user_id = ? AND scope = ?`,
		user.ID, data.ScopeAuthentication,
	).Scan(&stored)
	assert.NilError(t, err)

	want := sha256.Sum256([]byte(cfg.fixedToken))

	assert.Equal(t, hex.EncodeToString(stored), hex.EncodeToString(want[:]))
}

// TestRunIsIdempotent covers the re-runnable paths — the duplicate-email
// branch, the non-empty-movies branch, and the token cleanup.
//
// This is the behaviour the command's comments promise, and it's easy to break:
// an Insert that didn't handle ErrDuplicateEmail would fail the second run, and
// a token insert without the DeleteAllForUser first would accumulate rows.
func TestRunIsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database-backed test with -short")
	}

	cfg := testConfig(t)

	assert.NilError(t, run(cfg, &bytes.Buffer{}))

	var second bytes.Buffer
	assert.NilError(t, run(cfg, &second))

	output := second.String()
	assert.StringContains(t, output, "already exists, reusing it")
	assert.StringContains(t, output, "movies table is not empty, skipping sample movies")

	models := openSeeded(t, cfg)

	// Still exactly six movies, not twelve.
	movies, _, err := models.Movies.GetAll("", nil, data.Filters{
		Page: 1, PageSize: 100, Sort: "id", SortSafelist: []string{"id"},
	})
	assert.NilError(t, err)
	assert.Equal(t, len(movies), 6)

	// And the token still works after being re-seeded.
	user, err := models.Users.GetForToken(data.ScopeAuthentication, cfg.fixedToken)
	assert.NilError(t, err)
	assert.Equal(t, user.Email, cfg.email)

	// Exactly one authentication token row — the old one was burned.
	var count int
	database := openDB(t, cfg)
	err = database.QueryRow(
		`SELECT count(*) FROM tokens WHERE user_id = ? AND scope = ?`,
		user.ID, data.ScopeAuthentication,
	).Scan(&count)
	assert.NilError(t, err)
	assert.Equal(t, count, 1)
}

// TestRunRejectsAnInvalidToken covers the validation guard.
//
// The token must be exactly 26 characters, because that's the length
// TokenModel.New produces and what ValidateTokenPlaintext enforces on the
// activation endpoint. A shorter one would be inserted happily and then be
// rejected by the API, which is a confusing way to fail.
func TestRunRejectsAnInvalidToken(t *testing.T) {
	cfg := testConfig(t)
	cfg.fixedToken = "TOO-SHORT"

	err := run(cfg, &bytes.Buffer{})
	if err == nil {
		t.Fatal("got nil error for a 9-character token; want a validation failure")
	}

	assert.StringContains(t, err.Error(), "invalid -token")

	// It must fail BEFORE creating the database file, so a bad invocation
	// doesn't leave a half-seeded database behind.
	if _, statErr := os.Stat(cfg.dsn); statErr == nil {
		t.Error("a database file was created despite the token being rejected")
	}
}

// TestParseFlags covers the flag wiring, including the defaults the README and
// the Bruno collection depend on.
func TestParseFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := parseFlags(nil)
		assert.NilError(t, err)

		assert.Equal(t, cfg.dsn, "greenlight.db")
		assert.Equal(t, cfg.email, "demo@example.com")
		assert.Equal(t, cfg.password, "pa55word1234")
		// Hardcoded in bruno/environments/Local.bru — changing it breaks the
		// API collection.
		assert.Equal(t, cfg.fixedToken, "GREENLIGHT0000000000000000")
		assert.Equal(t, len(cfg.fixedToken), 26)
	})

	t.Run("overrides", func(t *testing.T) {
		cfg, err := parseFlags([]string{
			"-db-dsn", "/tmp/other.db",
			"-email", "other@example.com",
			"-password", "somethingelse",
			"-token", "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		})
		assert.NilError(t, err)

		assert.Equal(t, cfg.dsn, "/tmp/other.db")
		assert.Equal(t, cfg.email, "other@example.com")
		assert.Equal(t, cfg.password, "somethingelse")
		assert.Equal(t, cfg.fixedToken, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	})

	t.Run("unknown flag returns an error rather than exiting", func(t *testing.T) {
		// The FlagSet writes its usage message somewhere; silence it so a
		// passing test run stays readable.
		_, err := parseFlags([]string{"-nonexistent"})
		if err == nil {
			t.Fatal("got nil error for an unknown flag; want a parse failure")
		}
	})
}

// testConfig returns a config pointing at a fresh database in a temp directory.
func testConfig(t *testing.T) config {
	t.Helper()

	return config{
		dsn:        filepath.Join(t.TempDir(), "seed-test.db"),
		email:      "demo@example.com",
		password:   "pa55word1234",
		fixedToken: "GREENLIGHT0000000000000000",
	}
}

// openDB reopens the database the seeder wrote.
func openDB(t *testing.T, cfg config) *sql.DB {
	t.Helper()

	database, err := db.OpenDB(db.Config{DSN: cfg.dsn, MaxOpenConns: 2, MaxIdleConns: 2})
	assert.NilError(t, err)

	t.Cleanup(func() { database.Close() })

	return database
}

// openSeeded wraps it in the real model layer, so assertions go through the
// same code the API uses rather than raw SQL.
func openSeeded(t *testing.T, cfg config) data.Models {
	t.Helper()

	return data.NewModels(openDB(t, cfg))
}
