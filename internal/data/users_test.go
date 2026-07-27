package data

import (
	"errors"
	"testing"
	"time"

	"greenlight/internal/assert"
	"greenlight/internal/validator"
)

// newTestUser creates and inserts a user with a known password.
func newTestUser(t *testing.T, m UserModel, name, email, plaintext string) *User {
	t.Helper()

	user := &User{Name: name, Email: email, Activated: true}
	assert.NilError(t, user.Password.Set(plaintext))
	assert.NilError(t, m.Insert(user))

	return user
}

func TestPassword_SetAndMatches(t *testing.T) {
	var p password

	assert.NilError(t, p.Set("pa55word1234"))

	// The hash must not be the plaintext — a trivial check, but it's the one
	// mistake you absolutely cannot ship.
	if string(p.Hash()) == "pa55word1234" {
		t.Fatal("password was stored in plaintext")
	}

	match, err := p.Matches("pa55word1234")
	assert.NilError(t, err)
	assert.Equal(t, match, true)

	// A wrong password is a normal outcome, not an error: (false, nil).
	match, err = p.Matches("wrong-password")
	assert.NilError(t, err)
	assert.Equal(t, match, false)
}

// TestPassword_SaltingProducesDifferentHashes documents why we can't just hash
// and compare strings: bcrypt embeds a random salt, so the same password
// produces a different hash every time.
func TestPassword_SaltingProducesDifferentHashes(t *testing.T) {
	var a, b password

	assert.NilError(t, a.Set("pa55word1234"))
	assert.NilError(t, b.Set("pa55word1234"))

	if string(a.Hash()) == string(b.Hash()) {
		t.Error("identical passwords produced identical hashes; the salt isn't random")
	}

	// Both must still verify against the original plaintext.
	for _, p := range []password{a, b} {
		match, err := p.Matches("pa55word1234")
		assert.NilError(t, err)
		assert.Equal(t, match, true)
	}
}

func TestUserModel_InsertAndGetByEmail(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	if user.ID < 1 {
		t.Errorf("expected Insert to set an ID; got %d", user.ID)
	}
	assert.Equal(t, user.Version, 1)

	got, err := models.Users.GetByEmail("alice@example.com")
	assert.NilError(t, err)

	assert.Equal(t, got.ID, user.ID)
	assert.Equal(t, got.Name, "Alice")
	assert.Equal(t, got.Email, "alice@example.com")
	assert.Equal(t, got.Activated, true)

	// The hash must survive the BLOB round trip intact, or nobody could ever
	// log in again.
	match, err := got.Password.Matches("pa55word1234")
	assert.NilError(t, err)
	assert.Equal(t, match, true)
}

// TestUserModel_EmailIsCaseInsensitive verifies the `COLLATE NOCASE` column,
// which is our replacement for the book's Postgres `citext` type.
func TestUserModel_EmailIsCaseInsensitive(t *testing.T) {
	models := newTestModels(t)

	newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	// Lookups must match regardless of case.
	for _, email := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", "Alice@Example.Com"} {
		got, err := models.Users.GetByEmail(email)
		if err != nil {
			t.Errorf("looking up %q: got error %v; want a match", email, err)
			continue
		}
		assert.Equal(t, got.Name, "Alice")
	}

	// And registering the same address in a different case must be rejected as
	// a duplicate — otherwise you could shadow another user's account.
	dupe := &User{Name: "Impostor", Email: "ALICE@EXAMPLE.COM"}
	assert.NilError(t, dupe.Password.Set("pa55word1234"))

	err := models.Users.Insert(dupe)
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Errorf("got error %v; want ErrDuplicateEmail", err)
	}
}

func TestUserModel_InsertDuplicateEmail(t *testing.T) {
	models := newTestModels(t)

	newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	dupe := &User{Name: "Alice Again", Email: "alice@example.com"}
	assert.NilError(t, dupe.Password.Set("pa55word1234"))

	// This is the test that pins down isUniqueConstraintErr — if the SQLite
	// error code detection breaks, this returns a raw driver error instead.
	err := models.Users.Insert(dupe)
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Errorf("got error %v; want ErrDuplicateEmail", err)
	}
}

func TestUserModel_GetByEmailNotFound(t *testing.T) {
	models := newTestModels(t)

	_, err := models.Users.GetByEmail("nobody@example.com")
	if !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("got error %v; want ErrRecordNotFound", err)
	}
}

func TestUserModel_Update(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")
	user.Activated = false
	user.Name = "Alice Smith"

	assert.NilError(t, models.Users.Update(user))
	assert.Equal(t, user.Version, 2)

	got, err := models.Users.GetByEmail("alice@example.com")
	assert.NilError(t, err)
	assert.Equal(t, got.Name, "Alice Smith")
	assert.Equal(t, got.Activated, false)
}

func TestUserModel_UpdateEditConflict(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	stale, err := models.Users.GetByEmail("alice@example.com")
	assert.NilError(t, err)

	// A first update takes the record to version 2.
	user.Name = "Alice One"
	assert.NilError(t, models.Users.Update(user))

	// The stale copy is still on version 1, so its update must be rejected.
	stale.Name = "Alice Two"
	err = models.Users.Update(stale)
	if !errors.Is(err, ErrEditConflict) {
		t.Errorf("got error %v; want ErrEditConflict", err)
	}
}

func TestUserModel_UpdateDuplicateEmail(t *testing.T) {
	models := newTestModels(t)

	newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")
	bob := newTestUser(t, models.Users, "Bob", "bob@example.com", "pa55word1234")

	// Bob tries to take Alice's address.
	bob.Email = "alice@example.com"

	err := models.Users.Update(bob)
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Errorf("got error %v; want ErrDuplicateEmail", err)
	}
}

// TestUserModel_GetForToken exercises the authentication lookup, including the
// two ways it must FAIL: wrong scope and expired token. Those are security
// boundaries, so they matter more than the happy path.
func TestUserModel_GetForToken(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	authToken, err := models.Tokens.New(user.ID, 24*time.Hour, ScopeAuthentication)
	assert.NilError(t, err)

	t.Run("valid token", func(t *testing.T) {
		got, err := models.Users.GetForToken(ScopeAuthentication, authToken.Plaintext)
		assert.NilError(t, err)
		assert.Equal(t, got.ID, user.ID)
		assert.Equal(t, got.Email, "alice@example.com")
	})

	t.Run("wrong scope is rejected", func(t *testing.T) {
		// An activation token must never work as a login credential, and vice
		// versa. This is what stops a token emailed in the clear from being
		// replayed for full API access.
		_, err := models.Users.GetForToken(ScopeActivation, authToken.Plaintext)
		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("got error %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("unknown token is rejected", func(t *testing.T) {
		_, err := models.Users.GetForToken(ScopeAuthentication, "AAAAAAAAAAAAAAAAAAAAAAAAAA")
		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("got error %v; want ErrRecordNotFound", err)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		// A negative TTL puts the expiry in the past. This verifies both the
		// `expiry > ?` comparison AND that Go time.Time values round-trip
		// through the TIMESTAMP column correctly — if the driver mangled them,
		// this comparison would silently misbehave.
		expired, err := models.Tokens.New(user.ID, -time.Hour, ScopeAuthentication)
		assert.NilError(t, err)

		_, err = models.Users.GetForToken(ScopeAuthentication, expired.Plaintext)
		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("got error %v; want ErrRecordNotFound for an expired token", err)
		}
	})
}

func TestValidateUser(t *testing.T) {
	valid := func() *User {
		u := &User{Name: "Alice", Email: "alice@example.com"}
		if err := u.Password.Set("pa55word1234"); err != nil {
			t.Fatal(err)
		}
		return u
	}

	tests := []struct {
		name    string
		modify  func(*User)
		wantKey string
	}{
		{name: "valid user", modify: func(*User) {}},
		{name: "empty name", modify: func(u *User) { u.Name = "" }, wantKey: "name"},
		{name: "name too long", modify: func(u *User) { u.Name = string(make([]byte, 501)) }, wantKey: "name"},
		{name: "empty email", modify: func(u *User) { u.Email = "" }, wantKey: "email"},
		{name: "malformed email", modify: func(u *User) { u.Email = "not-an-email" }, wantKey: "email"},
		{name: "short password", modify: func(u *User) {
			assert.NilError(t, u.Password.Set("short"))
		}, wantKey: "password"},
		{name: "password over bcrypt's 72-byte limit", modify: func(u *User) {
			long := string(make([]byte, 73))
			// bcrypt itself rejects >72 bytes, so set the field directly to
			// isolate the validator.
			u.Password.plaintext = &long
		}, wantKey: "password"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := valid()
			tt.modify(user)

			v := validator.New()
			ValidateUser(v, user)

			if tt.wantKey == "" {
				if !v.Valid() {
					t.Errorf("expected user to be valid; got errors: %v", v.Errors)
				}
				return
			}

			if _, ok := v.Errors[tt.wantKey]; !ok {
				t.Errorf("expected an error on %q; got errors: %v", tt.wantKey, v.Errors)
			}
		})
	}
}

func TestAnonymousUser(t *testing.T) {
	assert.Equal(t, AnonymousUser.IsAnonymous(), true)

	// A real user, even a zero-valued one, must not be anonymous — the check
	// is pointer identity, not value equality.
	assert.Equal(t, (&User{}).IsAnonymous(), false)
	assert.Equal(t, (&User{Name: "Alice"}).IsAnonymous(), false)
}
