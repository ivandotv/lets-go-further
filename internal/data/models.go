// Package data contains the "model" layer: the Go types that represent our
// domain objects, and the code that reads and writes them to the database.
//
// The important architectural idea here is that this package knows about SQL
// but knows *nothing* about HTTP. It never sees a http.Request, never writes a
// response, never thinks about status codes. Handlers in cmd/api call these
// methods and translate the results into HTTP. That separation is what makes
// the models straightforward to unit-test.
package data

import (
	"database/sql"
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Sentinel errors returned by the models.
//
// "Sentinel" means these are specific, comparable error values that callers
// check for with errors.Is(). They form part of the model layer's API: a
// handler can react to ErrRecordNotFound with a 404 without having to know
// anything about SQL.
var (
	// ErrRecordNotFound is returned when a lookup matches no rows.
	// We define our own rather than leaking database/sql's ErrNoRows upward,
	// so handlers don't need to import database/sql.
	ErrRecordNotFound = errors.New("record not found")

	// ErrEditConflict is returned when an optimistic-locking check fails —
	// i.e. someone else updated the record between our read and our write.
	// See MovieModel.Update for how that works.
	ErrEditConflict = errors.New("edit conflict")
)

// Models wraps all the individual models in a single struct.
//
// This exists purely for convenience of dependency injection: the application
// struct holds one Models value, and handlers reach through it
// (app.models.Movies.Get(...)). Adding a new model later means adding one field
// here, not threading another parameter through the whole app.
type Models struct {
	Movies      MovieModel
	Users       UserModel
	Tokens      TokenModel
	Permissions PermissionModel
}

// NewModels returns a Models struct with every model wired up to the same
// connection pool.
func NewModels(db *sql.DB) Models {
	return Models{
		Movies:      MovieModel{DB: db},
		Users:       UserModel{DB: db},
		Tokens:      TokenModel{DB: db},
		Permissions: PermissionModel{DB: db},
	}
}

// isUniqueConstraintErr reports whether err is SQLite's "UNIQUE constraint
// failed" error.
//
// ─────────────────────────────────────────────────────────────────────────────
// POSTGRES → SQLITE DIFFERENCE
// ─────────────────────────────────────────────────────────────────────────────
//
// The book detects a duplicate email by string-matching the driver's message:
//
//	case err.Error() == `pq: duplicate key value violates unique constraint "users_email_key"`:
//
// That works, but it's brittle — it breaks if the constraint is renamed or the
// driver reformats its messages.
//
// modernc.org/sqlite gives us something better: a typed *sqlite.Error carrying
// SQLite's numeric extended result code. Code 2067 is
// SQLITE_CONSTRAINT_UNIQUE, and 1555 is SQLITE_CONSTRAINT_PRIMARYKEY (which is
// what you get when the clashing column is the primary key, as with our tokens
// table). We accept both.
//
// errors.As walks the error chain looking for one that can be assigned to the
// target — so this keeps working even if the error gets wrapped with %w
// somewhere along the way.
func isUniqueConstraintErr(err error) bool {
	var sErr *sqlite.Error
	if !errors.As(err, &sErr) {
		return false
	}

	code := sErr.Code()

	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
		code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}
