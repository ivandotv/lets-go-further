package data

import (
	"crypto/sha256"
	"testing"
	"time"

	"greenlight/internal/assert"
	"greenlight/internal/validator"
)

func TestGenerateToken(t *testing.T) {
	token, err := generateToken(42, 3*time.Hour, ScopeActivation)
	assert.NilError(t, err)

	assert.Equal(t, token.UserID, 42)
	assert.Equal(t, token.Scope, ScopeActivation)

	// 16 random bytes base32-encoded without padding is always 26 characters.
	// ValidateTokenPlaintext depends on this exact length, so it's worth
	// asserting rather than assuming.
	assert.Equal(t, len(token.Plaintext), 26)

	// The stored hash must be the SHA-256 of the plaintext.
	want := sha256.Sum256([]byte(token.Plaintext))
	assert.Equal(t, string(token.Hash), string(want[:]))

	// And it must NOT be the plaintext itself — the whole point is that a
	// database leak yields nothing usable.
	if string(token.Hash) == token.Plaintext {
		t.Fatal("token was stored in plaintext")
	}

	// Expiry should be roughly now+ttl.
	wantExpiry := time.Now().Add(3 * time.Hour)
	if token.Expiry.Sub(wantExpiry).Abs() > time.Minute {
		t.Errorf("got expiry %v; want ~%v", token.Expiry, wantExpiry)
	}
}

// TestGenerateTokenIsRandom is a smoke test for the CSPRNG. If someone
// "optimised" crypto/rand into math/rand with a fixed seed, this catches it.
func TestGenerateTokenIsRandom(t *testing.T) {
	seen := make(map[string]struct{})

	for range 100 {
		token, err := generateToken(1, time.Hour, ScopeActivation)
		assert.NilError(t, err)

		if _, dupe := seen[token.Plaintext]; dupe {
			t.Fatalf("generated a duplicate token %q within 100 attempts", token.Plaintext)
		}
		seen[token.Plaintext] = struct{}{}
	}
}

func TestTokenModel_NewAndDeleteAllForUser(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	// Issue one token of each scope.
	activation, err := models.Tokens.New(user.ID, time.Hour, ScopeActivation)
	assert.NilError(t, err)

	auth, err := models.Tokens.New(user.ID, time.Hour, ScopeAuthentication)
	assert.NilError(t, err)

	// Both work to start with.
	_, err = models.Users.GetForToken(ScopeActivation, activation.Plaintext)
	assert.NilError(t, err)
	_, err = models.Users.GetForToken(ScopeAuthentication, auth.Plaintext)
	assert.NilError(t, err)

	// Deleting the activation scope must burn ONLY the activation token. This
	// is exactly what activateUserHandler relies on: activating your account
	// shouldn't log you out.
	assert.NilError(t, models.Tokens.DeleteAllForUser(ScopeActivation, user.ID))

	if _, err := models.Users.GetForToken(ScopeActivation, activation.Plaintext); err == nil {
		t.Error("activation token still works after DeleteAllForUser")
	}

	if _, err := models.Users.GetForToken(ScopeAuthentication, auth.Plaintext); err != nil {
		t.Errorf("authentication token was deleted too: %v", err)
	}
}

func TestTokenModel_DeleteExpired(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	// Two expired, one live.
	_, err := models.Tokens.New(user.ID, -time.Hour, ScopeAuthentication)
	assert.NilError(t, err)
	_, err = models.Tokens.New(user.ID, -time.Minute, ScopeActivation)
	assert.NilError(t, err)
	live, err := models.Tokens.New(user.ID, time.Hour, ScopeAuthentication)
	assert.NilError(t, err)

	deleted, err := models.Tokens.DeleteExpired()
	assert.NilError(t, err)
	assert.Equal(t, deleted, int64(2))

	// The live token must survive.
	_, err = models.Users.GetForToken(ScopeAuthentication, live.Plaintext)
	assert.NilError(t, err)
}

// TestTokenModel_CascadeDeleteOnUser verifies that the ON DELETE CASCADE
// foreign key actually fires.
//
// This is a genuinely important SQLite test: foreign keys are DISABLED by
// default in SQLite, per connection. If the `_pragma=foreign_keys(1)` in our
// DSN were ever dropped, this constraint would silently become decorative and
// deleted users would leave live tokens behind — a real security problem. This
// test would catch that.
func TestTokenModel_CascadeDeleteOnUser(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	token, err := models.Tokens.New(user.ID, time.Hour, ScopeAuthentication)
	assert.NilError(t, err)

	// There's no DeleteUser model method, so go direct.
	_, err = models.Users.DB.Exec("DELETE FROM users WHERE id = ?", user.ID)
	assert.NilError(t, err)

	var count int
	err = models.Tokens.DB.QueryRow("SELECT count(*) FROM tokens WHERE user_id = ?", user.ID).Scan(&count)
	assert.NilError(t, err)

	if count != 0 {
		t.Errorf("got %d orphaned tokens after deleting the user; want 0 (is the foreign_keys pragma set?)", count)
	}

	// And the token must no longer authenticate.
	if _, err := models.Users.GetForToken(ScopeAuthentication, token.Plaintext); err == nil {
		t.Error("token still authenticates after its user was deleted")
	}
}

// TestValidateTokenPlaintext pins the length check that runs before any
// database lookup.
//
// The only rule is "exactly 26 characters", which is what base32-encoding 16
// random bytes always produces (see TestGenerateToken above). Checking it up
// front means an obviously-malformed token is rejected with a clean 422 rather
// than costing a SHA-256 and a query — worth having on an endpoint anyone can
// call without credentials.
//
// Note what is deliberately NOT validated: whether the characters are valid
// base32. A wrong-but-well-formed token has to be looked up anyway, so there's
// nothing to gain from checking the alphabet too.
func TestValidateTokenPlaintext(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		wantValid bool
	}{
		{name: "correct length", token: "AAAAAAAAAAAAAAAAAAAAAAAAAA", wantValid: true},
		{name: "empty", token: "", wantValid: false},
		{name: "too short", token: "AAAA", wantValid: false},
		{name: "too long", token: "AAAAAAAAAAAAAAAAAAAAAAAAAAA", wantValid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := validator.New()
			ValidateTokenPlaintext(v, tt.token)
			assert.Equal(t, v.Valid(), tt.wantValid)
		})
	}
}
