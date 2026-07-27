package data

import (
	"slices"
	"testing"

	"greenlight/internal/assert"
)

func TestPermissions_Include(t *testing.T) {
	p := Permissions{"movies:read", "movies:write"}

	assert.Equal(t, p.Include("movies:read"), true)
	assert.Equal(t, p.Include("movies:write"), true)
	assert.Equal(t, p.Include("movies:delete"), false)
	assert.Equal(t, p.Include(""), false)

	// An empty permission set includes nothing — important, because that's the
	// state of a brand-new user before AddForUser runs.
	assert.Equal(t, Permissions{}.Include("movies:read"), false)
}

func TestPermissionModel_AddAndGetAllForUser(t *testing.T) {
	models := newTestModels(t)

	alice := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")
	bob := newTestUser(t, models.Users, "Bob", "bob@example.com", "pa55word1234")

	// A new user starts with nothing.
	got, err := models.Permissions.GetAllForUser(alice.ID)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 0)
	// Must be an empty slice, not nil, so callers can range over it safely.
	if got == nil {
		t.Error("got a nil Permissions slice; want an empty one")
	}

	// Grant one permission.
	assert.NilError(t, models.Permissions.AddForUser(alice.ID, PermissionMoviesRead))

	got, err = models.Permissions.GetAllForUser(alice.ID)
	assert.NilError(t, err)
	if !slices.Equal(got, Permissions{PermissionMoviesRead}) {
		t.Errorf("got %v; want [movies:read]", got)
	}

	// Grant several at once — this is the variadic/JSON1 path.
	assert.NilError(t, models.Permissions.AddForUser(bob.ID, PermissionMoviesRead, PermissionMoviesWrite))

	got, err = models.Permissions.GetAllForUser(bob.ID)
	assert.NilError(t, err)
	slices.Sort(got)
	if !slices.Equal(got, Permissions{PermissionMoviesRead, PermissionMoviesWrite}) {
		t.Errorf("got %v; want [movies:read movies:write]", got)
	}

	// Permissions must not leak between users: Alice still has only one.
	got, err = models.Permissions.GetAllForUser(alice.ID)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
}

// TestPermissionModel_AddForUserIsIdempotent verifies the `INSERT OR IGNORE`.
//
// Without it, granting a permission twice would violate the composite primary
// key on users_permissions and return an error — which would make any "sync
// this user's permissions" operation fragile.
func TestPermissionModel_AddForUserIsIdempotent(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	assert.NilError(t, models.Permissions.AddForUser(user.ID, PermissionMoviesRead))
	// The same grant a second time must be a silent no-op.
	assert.NilError(t, models.Permissions.AddForUser(user.ID, PermissionMoviesRead))

	got, err := models.Permissions.GetAllForUser(user.ID)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 1)
}

// TestPermissionModel_AddForUserUnknownCode documents the behaviour when a
// code doesn't exist in the permissions table: the INSERT ... SELECT simply
// selects no rows, so nothing is granted and no error is raised.
//
// That's a quiet failure mode, which is exactly why ValidPermissionCode exists
// for any caller that accepts a code from outside.
func TestPermissionModel_AddForUserUnknownCode(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	assert.NilError(t, models.Permissions.AddForUser(user.ID, "movies:teleport"))

	got, err := models.Permissions.GetAllForUser(user.ID)
	assert.NilError(t, err)
	assert.Equal(t, len(got), 0)
}

func TestValidPermissionCode(t *testing.T) {
	assert.Equal(t, ValidPermissionCode(PermissionMoviesRead), true)
	assert.Equal(t, ValidPermissionCode(PermissionMoviesWrite), true)
	assert.Equal(t, ValidPermissionCode(" movies:read "), true)
	assert.Equal(t, ValidPermissionCode("movies:teleport"), false)
	assert.Equal(t, ValidPermissionCode(""), false)
}

// TestPermissionModel_CascadeDeleteOnUser checks that removing a user also
// removes their permission grants, rather than leaving orphaned rows that a
// future user with the same ID could inherit.
func TestPermissionModel_CascadeDeleteOnUser(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")
	assert.NilError(t, models.Permissions.AddForUser(user.ID, PermissionMoviesRead))

	_, err := models.Users.DB.Exec("DELETE FROM users WHERE id = ?", user.ID)
	assert.NilError(t, err)

	var count int
	err = models.Permissions.DB.QueryRow(
		"SELECT count(*) FROM users_permissions WHERE user_id = ?", user.ID,
	).Scan(&count)
	assert.NilError(t, err)

	assert.Equal(t, count, 0)
}
