package data

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"
)

// Permission codes used by the API.
//
// The "resource:action" convention scales nicely — adding `reviews:write`
// later needs no code changes beyond a migration row and a route.
const (
	PermissionMoviesRead  = "movies:read"
	PermissionMoviesWrite = "movies:write"
)

// Permissions is the set of permission codes held by a user.
type Permissions []string

// Include reports whether the set contains a specific code.
func (p Permissions) Include(code string) bool {
	return slices.Contains(p, code)
}

// PermissionModel wraps the connection pool.
type PermissionModel struct {
	DB *sql.DB
}

// GetAllForUser returns every permission code for a given user.
//
// The query walks the many-to-many relationship:
//
//	permissions ← users_permissions → users
//
// Two INNER JOINs get us from a user id to the permission codes.
func (m PermissionModel) GetAllForUser(userID int64) (Permissions, error) {
	query := `
		SELECT permissions.code
		FROM permissions
		INNER JOIN users_permissions ON users_permissions.permission_id = permissions.id
		INNER JOIN users ON users_permissions.user_id = users.id
		WHERE users.id = ?`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := m.DB.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	// Start with an empty non-nil slice so callers can range over the result
	// without a nil check.
	permissions := Permissions{}

	for rows.Next() {
		var permission string

		if err := rows.Scan(&permission); err != nil {
			return nil, err
		}

		permissions = append(permissions, permission)
	}

	// Always check rows.Err() after the loop — see the note in movies.GetAll.
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return permissions, nil
}

// AddForUser grants one or more permissions to a user.
//
// ── POSTGRES → SQLITE DIFFERENCE ─────────────────────────────────────────────
//
// The book writes this as a single statement using a Postgres array:
//
//	INSERT INTO users_permissions
//	SELECT $1, permissions.id FROM permissions WHERE permissions.code = ANY($2)
//
// SQLite has no `= ANY(array)`. Two options: build a query with N placeholders,
// or use the JSON1 extension the way we did for genre filtering. We use JSON1
// again for consistency — json_each() turns the JSON array of codes into rows
// we can join against.
//
// `INSERT OR IGNORE` makes the operation idempotent: granting a permission a
// user already has is a no-op rather than a UNIQUE constraint violation. (In
// Postgres this would be `ON CONFLICT DO NOTHING`.)
func (m PermissionModel) AddForUser(userID int64, codes ...string) error {
	query := `
		INSERT OR IGNORE INTO users_permissions (user_id, permission_id)
		SELECT ?, permissions.id
		FROM permissions
		WHERE permissions.code IN (SELECT value FROM json_each(?))`

	codesJSON, err := Genres(codes).Value()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = m.DB.ExecContext(ctx, query, userID, codesJSON)

	return err
}

// ValidPermissionCode reports whether a code is one the API knows about.
//
// Handy for guarding an admin endpoint that grants permissions, so a typo
// doesn't silently create a permission that can never be satisfied.
func ValidPermissionCode(code string) bool {
	return slices.Contains([]string{PermissionMoviesRead, PermissionMoviesWrite}, strings.TrimSpace(code))
}
