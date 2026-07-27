package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"golang.org/x/crypto/bcrypt"

	"greenlight/internal/validator"
)

// ErrDuplicateEmail is returned when an email address is already registered.
// We surface this as its own error so the handler can attach the message to
// the "email" field of a 422 response rather than returning a generic 500.
var ErrDuplicateEmail = errors.New("duplicate email")

// AnonymousUser represents a request with no valid authentication token.
//
// Using a non-nil sentinel *User rather than nil is a deliberate choice: it
// means middleware and handlers can call methods on it (u.IsAnonymous(),
// u.Activated) without nil-checking first. No nil pointer dereferences.
var AnonymousUser = &User{}

// User is a registered user of the API.
type User struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`

	// The `json:"-"` tag is doing critical security work here: it guarantees
	// the password hash can never be accidentally included in a response, no
	// matter which handler encodes a User.
	Password password `json:"-"`

	Activated bool `json:"activated"`
	Version   int  `json:"-"`
}

// IsAnonymous reports whether this User is the AnonymousUser sentinel.
//
// The comparison is by POINTER identity (==), not by value. That's the point:
// a real user who happens to have all-zero fields would still not be
// anonymous.
func (u *User) IsAnonymous() bool {
	return u == AnonymousUser
}

// password holds a bcrypt hash and, transiently, the plaintext it came from.
//
// The type is unexported and both fields are unexported, so the only way to
// populate it is through Set(). That makes it impossible for a caller
// elsewhere in the codebase to accidentally store an unhashed password.
type password struct {
	// plaintext is a *pointer* so we can distinguish three states:
	//   nil            → no password supplied in this request (e.g. a PATCH
	//                    that doesn't touch the password)
	//   pointer to ""  → an empty password was explicitly supplied (invalid)
	//   pointer to "x" → a real password was supplied
	// A plain string could only express two of those.
	plaintext *string
	hash      []byte
}

// BcryptCost is the work factor used when hashing passwords.
//
// Cost 12 means bcrypt runs 2^12 rounds, which takes roughly a quarter of a
// second on modern hardware. That slowness is the entire point — it makes
// offline brute-forcing of a stolen hash table prohibitively expensive.
//
// It's an exported `var` rather than a `const` for exactly one reason: test
// suites lower it to bcrypt.MinCost so they don't spend their whole runtime
// deliberately burning CPU. (Our own suite drops a 9-second run to under a
// second this way.) Production code must never assign to it.
//
// This is safe because bcrypt stores the cost factor inside each hash, so
// verification works regardless of the cost used to create it.
var BcryptCost = 12

// Set hashes the plaintext password with bcrypt and stores both.
func (p *password) Set(plaintextPassword string) error {
	// bcrypt generates and embeds a random salt automatically, so identical
	// passwords produce different hashes.
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintextPassword), BcryptCost)
	if err != nil {
		return err
	}

	p.plaintext = &plaintextPassword
	p.hash = hash

	return nil
}

// Matches reports whether the supplied plaintext password matches the stored
// hash.
//
// Note we can't just hash the input and compare — bcrypt salts each hash, so
// the same password hashes differently every time. CompareHashAndPassword
// extracts the salt and cost from the stored hash and redoes the work.
func (p *password) Matches(plaintextPassword string) (bool, error) {
	err := bcrypt.CompareHashAndPassword(p.hash, []byte(plaintextPassword))
	if err != nil {
		// A mismatch is an expected outcome, not a failure of the system, so
		// translate it to (false, nil). Anything else is a real error.
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// Hash exposes the raw bcrypt hash. It exists so tests can construct a User
// with a known hash without going through the database.
func (p *password) Hash() []byte {
	return p.hash
}

// ValidateEmail checks that an email address is present and plausibly formed.
func ValidateEmail(v *validator.Validator, email string) {
	v.Check(email != "", "email", "must be provided")
	v.Check(validator.Matches(email, validator.EmailRX), "email", "must be a valid email address")
}

// ValidatePasswordPlaintext checks password length.
//
// The 8-byte minimum and 72-byte maximum are not arbitrary: 72 bytes is a hard
// limit of the bcrypt algorithm itself, which silently ignores anything beyond
// it. Rejecting longer passwords is better than silently truncating them.
func ValidatePasswordPlaintext(v *validator.Validator, password string) {
	v.Check(password != "", "password", "must be provided")
	v.Check(len(password) >= 8, "password", "must be at least 8 bytes long")
	v.Check(len(password) <= 72, "password", "must not be more than 72 bytes long")
}

// ValidateUser runs every rule for a User.
func ValidateUser(v *validator.Validator, user *User) {
	v.Check(user.Name != "", "name", "must be provided")
	v.Check(len(user.Name) <= 500, "name", "must not be more than 500 bytes long")

	ValidateEmail(v, user.Email)

	// Only validate the plaintext if one was actually supplied.
	if user.Password.plaintext != nil {
		ValidatePasswordPlaintext(v, *user.Password.plaintext)
	}

	// A nil hash means Set() was never called — that's a bug in our code, not
	// bad input from a client, so panic rather than returning a validation
	// error a client can't act on.
	if user.Password.hash == nil {
		panic("missing password hash for user")
	}
}

// UserModel wraps the connection pool with the user CRUD methods.
type UserModel struct {
	DB *sql.DB
}

// Insert registers a new user.
func (m UserModel) Insert(user *User) error {
	query := `
		INSERT INTO users (name, email, password_hash, activated)
		VALUES (?, ?, ?, ?)
		RETURNING id, created_at, version`

	args := []any{user.Name, user.Email, user.Password.hash, user.Activated}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, args...).Scan(&user.ID, &user.CreatedAt, &user.Version)
	if err != nil {
		// The UNIQUE constraint on users.email is the only one on this table,
		// so a unique violation here can only mean a duplicate email.
		// See isUniqueConstraintErr in models.go for why we check it this way.
		if isUniqueConstraintErr(err) {
			return ErrDuplicateEmail
		}
		return err
	}

	return nil
}

// GetByEmail looks up a user by email address.
//
// The lookup is case-insensitive because the column is declared
// `COLLATE NOCASE` — SQLite's stand-in for the book's Postgres `citext` type.
func (m UserModel) GetByEmail(email string) (*User, error) {
	query := `
		SELECT id, created_at, name, email, password_hash, activated, version
		FROM users
		WHERE email = ?`

	var user User

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, email).Scan(
		&user.ID,
		&user.CreatedAt,
		&user.Name,
		&user.Email,
		&user.Password.hash,
		&user.Activated,
		&user.Version,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
		return nil, err
	}

	return &user, nil
}

// Update writes a modified user back, using the same optimistic-locking scheme
// as movies.
func (m UserModel) Update(user *User) error {
	query := `
		UPDATE users
		SET name = ?, email = ?, password_hash = ?, activated = ?, version = version + 1
		WHERE id = ? AND version = ?
		RETURNING version`

	args := []any{
		user.Name,
		user.Email,
		user.Password.hash,
		user.Activated,
		user.ID,
		user.Version,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, args...).Scan(&user.Version)
	if err != nil {
		switch {
		// Changing an email to one that's already taken.
		case isUniqueConstraintErr(err):
			return ErrDuplicateEmail
		case errors.Is(err, sql.ErrNoRows):
			return ErrEditConflict
		default:
			return err
		}
	}

	return nil
}

// GetForToken looks up the user associated with a given plaintext token,
// provided the token has the right scope and hasn't expired.
//
// This is the heart of authentication: the client sends
// `Authorization: Bearer <plaintext>`, and this single query turns it into a
// User.
func (m UserModel) GetForToken(tokenScope, tokenPlaintext string) (*User, error) {
	// We store only hashes, so hash the incoming token to find the row.
	//
	// sha256.Sum256 returns a fixed-size [32]byte array, not a slice. The
	// `[:]` converts it to a slice so it can be used as a query argument.
	//
	// Why SHA-256 here rather than bcrypt (as we use for passwords)? Because
	// these tokens are 128 bits of output from a CSPRNG — there is no
	// dictionary to attack, so the deliberate slowness of bcrypt buys nothing
	// and would add latency to every single authenticated request.
	tokenHash := sha256.Sum256([]byte(tokenPlaintext))

	query := `
		SELECT users.id, users.created_at, users.name, users.email,
		       users.password_hash, users.activated, users.version
		FROM users
		INNER JOIN tokens ON users.id = tokens.user_id
		WHERE tokens.hash = ?
		AND tokens.scope = ?
		AND tokens.expiry > ?`

	args := []any{tokenHash[:], tokenScope, time.Now()}

	var user User

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, args...).Scan(
		&user.ID,
		&user.CreatedAt,
		&user.Name,
		&user.Email,
		&user.Password.hash,
		&user.Activated,
		&user.Version,
	)
	if err != nil {
		// No row means: no such token, wrong scope, or expired. We deliberately
		// don't distinguish between those — telling an attacker *which* of
		// those it was would leak information.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
		return nil, err
	}

	return &user, nil
}
