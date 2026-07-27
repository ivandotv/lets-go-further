package data

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"time"

	"greenlight/internal/validator"
)

// Token scopes. A token is only accepted for the purpose it was issued for, so
// an activation token emailed to a new user can't be replayed as a login
// credential.
const (
	ScopeActivation     = "activation"
	ScopeAuthentication = "authentication"
)

// Token is a bearer credential.
type Token struct {
	// Plaintext is the value we send to the client. It exists only in memory,
	// only for as long as it takes to put it in an email or a response body —
	// it is never persisted.
	Plaintext string `json:"token"`

	// Hash is the SHA-256 of Plaintext, and is what actually goes in the
	// database. `json:"-"` keeps it out of responses.
	Hash []byte `json:"-"`

	UserID int64     `json:"-"`
	Expiry time.Time `json:"expiry"`
	Scope  string    `json:"-"`
}

// generateToken creates a cryptographically secure random token.
func generateToken(userID int64, ttl time.Duration, scope string) (*Token, error) {
	token := &Token{
		UserID: userID,
		Expiry: time.Now().Add(ttl),
		Scope:  scope,
	}

	// 16 bytes = 128 bits of entropy. That is far beyond guessable: even at a
	// billion attempts per second you'd need longer than the age of the
	// universe.
	randomBytes := make([]byte, 16)

	// crypto/rand — NOT math/rand. math/rand is deterministic and predictable
	// from a small amount of output; using it for anything security-related is
	// a serious vulnerability. crypto/rand reads from the OS CSPRNG.
	_, err := rand.Read(randomBytes)
	if err != nil {
		return nil, err
	}

	// Encode as base32 to get a string that's safe to paste into an email or
	// an HTTP header.
	//
	// WithPadding(NoPadding) drops the trailing '=' characters, which are
	// meaningless here and awkward in URLs. 16 bytes → 26 base32 characters.
	//
	// Base32 rather than base64 because base32's alphabet has no lowercase and
	// no easily-confused characters, which makes tokens far less error-prone
	// when a human has to retype one from an email.
	token.Plaintext = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomBytes)

	// Hash the plaintext for storage. If the database is ever compromised, the
	// attacker gets hashes they cannot reverse into usable tokens.
	hash := sha256.Sum256([]byte(token.Plaintext))
	token.Hash = hash[:]

	return token, nil
}

// ValidateTokenPlaintext checks that a supplied token is the right shape.
//
// This is a cheap sanity check before we hit the database — a token of the
// wrong length definitely isn't ours.
func ValidateTokenPlaintext(v *validator.Validator, tokenPlaintext string) {
	v.Check(tokenPlaintext != "", "token", "must be provided")
	v.Check(len(tokenPlaintext) == 26, "token", "must be 26 bytes long")
}

// TokenModel wraps the connection pool.
type TokenModel struct {
	DB *sql.DB
}

// New is a convenience wrapper that generates a token AND stores it.
//
// It returns the Token including its Plaintext, because that's the one moment
// the caller can read it — after this, only the hash exists.
func (m TokenModel) New(userID int64, ttl time.Duration, scope string) (*Token, error) {
	token, err := generateToken(userID, ttl, scope)
	if err != nil {
		return nil, err
	}

	err = m.Insert(token)

	return token, err
}

// Insert stores a token.
func (m TokenModel) Insert(token *Token) error {
	query := `
		INSERT INTO tokens (hash, user_id, expiry, scope)
		VALUES (?, ?, ?, ?)`

	args := []any{token.Hash, token.UserID, token.Expiry, token.Scope}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Note: token.Expiry is a Go time.Time, and the `expiry` column is declared
	// TIMESTAMP. The modernc.org/sqlite driver converts between the two
	// automatically based on that declared type, so the value round-trips
	// exactly — including surviving the `tokens.expiry > ?` comparison in
	// UserModel.GetForToken.
	_, err := m.DB.ExecContext(ctx, query, args...)

	return err
}

// DeleteAllForUser removes every token of a given scope belonging to a user.
//
// We call this in two places:
//   - after successful activation, to burn the activation token (and any
//     others that were issued if the user requested several emails)
//   - it's also the building block for a "log out everywhere" feature
func (m TokenModel) DeleteAllForUser(scope string, userID int64) error {
	query := `DELETE FROM tokens WHERE scope = ? AND user_id = ?`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := m.DB.ExecContext(ctx, query, scope, userID)

	return err
}

// DeleteExpired removes every token whose expiry has passed.
//
// Expired tokens are already rejected by the WHERE clause in GetForToken, so
// this is pure housekeeping to stop the table growing without bound. It's a
// natural thing to call from a periodic background job.
func (m TokenModel) DeleteExpired() (int64, error) {
	query := `DELETE FROM tokens WHERE expiry <= ?`

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := m.DB.ExecContext(ctx, query, time.Now())
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}
