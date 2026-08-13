package data

import (
	"errors"
	"testing"
	"time"

	"greenlight/internal/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// EDGE CASES
// ─────────────────────────────────────────────────────────────────────────────
//
// The tests in the other files cover each model's main path plus its most
// interesting failure. These pick up the branches that were left over: the
// guard clauses, the empty-result cases, and the housekeeping method.
//
// They're small, but the guard clauses in particular are load-bearing —
// Get(0) and Delete(0) return ErrRecordNotFound WITHOUT touching the database,
// which is what lets the handlers treat "/v1/movies/0" as a 404 rather than a
// query.
//
// ── errors.Is, AND WHY NOT == ────────────────────────────────────────────────
//
// The assertions below use errors.Is(err, ErrRecordNotFound) rather than
// err == ErrRecordNotFound. Both work today, because the model layer returns
// these sentinel values directly. errors.Is is still the right habit: it walks
// the chain of wrapped errors, so it keeps working the moment anyone writes
// fmt.Errorf("...: %w", ErrRecordNotFound). The %w verb is what does the
// wrapping — %v would flatten the error to a string and break Is entirely.

// TestMovieModel_GetRejectsNonPositiveIDs covers the short-circuit in Get.
func TestMovieModel_GetRejectsNonPositiveIDs(t *testing.T) {
	models := newTestModels(t)

	for _, id := range []int64{0, -1, -9999} {
		movie, err := models.Movies.Get(id)

		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("Get(%d): got error %v; want ErrRecordNotFound", id, err)
		}
		if movie != nil {
			t.Errorf("Get(%d): got a movie %+v; want nil", id, movie)
		}
	}
}

// TestMovieModel_DeleteRejectsNonPositiveIDs covers the same guard in Delete.
//
// This one matters slightly more than Get's: without it, `DELETE FROM movies
// WHERE id = 0` would run, match nothing, and return ErrRecordNotFound anyway —
// so the behaviour is identical but a pointless write-locking query is issued.
// On SQLite, where writes serialise globally, that's not free.
func TestMovieModel_DeleteRejectsNonPositiveIDs(t *testing.T) {
	models := newTestModels(t)

	// Insert a movie so the table isn't empty — if the guard were removed and
	// the query somehow matched everything, this would still be here.
	movie := &Movie{Title: "Untouched", Year: 2000, Runtime: 100, Genres: Genres{"drama"}}
	assert.NilError(t, models.Movies.Insert(movie))

	for _, id := range []int64{0, -1, -9999} {
		err := models.Movies.Delete(id)

		if !errors.Is(err, ErrRecordNotFound) {
			t.Errorf("Delete(%d): got error %v; want ErrRecordNotFound", id, err)
		}
	}

	// Still there.
	_, err := models.Movies.Get(movie.ID)
	assert.NilError(t, err)
}

// TestMovieModel_DeleteNonExistentID covers the rows-affected check — the
// branch where the query runs but matches nothing.
func TestMovieModel_DeleteNonExistentID(t *testing.T) {
	models := newTestModels(t)

	err := models.Movies.Delete(99999)

	assert.ErrorIs(t, err, ErrRecordNotFound)
}

// TestPermissionModel_GetAllForUserWithNone covers the empty-result path.
//
// The assertion that matters is non-nil, not empty. GetAllForUser starts with
// `Permissions{}` rather than a nil slice specifically so that callers — and
// json.Marshal — get `[]` instead of `null`. A nil here would change the API's
// output for every user with no permissions.
func TestPermissionModel_GetAllForUserWithNone(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Nobody", "nobody@example.com", "pa55word1234")

	permissions, err := models.Permissions.GetAllForUser(user.ID)
	assert.NilError(t, err)

	if permissions == nil {
		t.Fatal("got a nil Permissions; want an empty non-nil slice so it marshals to [] rather than null")
	}

	assert.Equal(t, len(permissions), 0)
	assert.Equal(t, permissions.Include(PermissionMoviesRead), false)
}

// TestPermissionModel_GetAllForUnknownUser checks that a user ID that doesn't
// exist behaves like one with no permissions, rather than erroring.
//
// This is what makes the requirePermission middleware safe: it can look up
// permissions for whatever user the token resolved to without a special case,
// and an unknown ID simply grants nothing.
func TestPermissionModel_GetAllForUnknownUser(t *testing.T) {
	models := newTestModels(t)

	permissions, err := models.Permissions.GetAllForUser(99999)
	assert.NilError(t, err)

	if permissions == nil {
		t.Fatal("got a nil Permissions for an unknown user; want an empty non-nil slice")
	}
	assert.Equal(t, len(permissions), 0)
}

// TestTokenModel_DeleteExpiredCountsAndSpares checks the housekeeping method
// removes exactly the expired tokens and reports how many.
//
// The count is the part worth pinning: it's the return value a cleanup job
// would log, and a method that quietly returned 0 while deleting rows (or vice
// versa) would be very hard to notice in production.
func TestTokenModel_DeleteExpiredCountsAndSpares(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Alice", "alice@example.com", "pa55word1234")

	// Three expired, two live.
	for range 3 {
		_, err := models.Tokens.New(user.ID, -1*time.Hour, ScopeAuthentication)
		assert.NilError(t, err)
	}

	live := make([]string, 0, 2)
	for range 2 {
		token, err := models.Tokens.New(user.ID, 24*time.Hour, ScopeAuthentication)
		assert.NilError(t, err)
		live = append(live, token.Plaintext)
	}

	deleted, err := models.Tokens.DeleteExpired()
	assert.NilError(t, err)
	assert.Equal(t, deleted, int64(3))

	// The live tokens still work.
	for _, plaintext := range live {
		_, err := models.Users.GetForToken(ScopeAuthentication, plaintext)
		assert.NilError(t, err)
	}

	// Running it again removes nothing.
	deleted, err = models.Tokens.DeleteExpired()
	assert.NilError(t, err)
	assert.Equal(t, deleted, int64(0))
}

// TestTokenModel_DeleteExpiredOnEmptyTable covers the trivial case, which is
// what a cleanup job would hit on every run but the first.
func TestTokenModel_DeleteExpiredOnEmptyTable(t *testing.T) {
	models := newTestModels(t)

	deleted, err := models.Tokens.DeleteExpired()
	assert.NilError(t, err)
	assert.Equal(t, deleted, int64(0))
}

// TestMovieModel_GetAllWithNoMatches checks the empty-result path of the list
// query.
//
// Like GetAllForUser above, the contract is an empty non-nil slice: the API
// returns `"movies": []` for a search with no hits, never `"movies": null`.
// There's an existing test for the no-filter case; this covers a filter that
// excludes everything, which goes down a different branch of the query builder.
func TestMovieModel_GetAllWithNoMatches(t *testing.T) {
	models := newTestModels(t)

	movie := &Movie{Title: "Casablanca", Year: 1942, Runtime: 102, Genres: Genres{"drama"}}
	assert.NilError(t, models.Movies.Insert(movie))

	filters := Filters{
		Page: 1, PageSize: 20, Sort: "id", SortSafelist: []string{"id"},
	}

	movies, metadata, err := models.Movies.GetAll("no such title", nil, filters)
	assert.NilError(t, err)

	if movies == nil {
		t.Fatal("got a nil slice; want an empty non-nil slice so it marshals to []")
	}
	assert.Equal(t, len(movies), 0)

	// An empty result set gets the zero Metadata, which marshals to {} rather
	// than a page count of 0 — the API says "there are no pages", not "there is
	// page 0 of 0".
	assert.DeepEqual(t, metadata, Metadata{})

	// A genre filter that matches nothing takes the same path.
	movies, _, err = models.Movies.GetAll("", []string{"nonexistent-genre"}, filters)
	assert.NilError(t, err)
	assert.Equal(t, len(movies), 0)
}

// TestMovieModel_GetAllPageBeyondResults checks asking for a page past the end.
//
// It's an easy off-by-one to get wrong in the LIMIT/OFFSET arithmetic, and the
// failure mode — an error or a panic rather than an empty page — would hit any
// client that paginates to the end.
func TestMovieModel_GetAllPageBeyondResults(t *testing.T) {
	models := newTestModels(t)

	for i := range 3 {
		movie := &Movie{
			Title: "Movie", Year: int32(2000 + i), Runtime: 100, Genres: Genres{"drama"},
		}
		assert.NilError(t, models.Movies.Insert(movie))
	}

	movies, metadata, err := models.Movies.GetAll("", nil, Filters{
		Page: 99, PageSize: 20, Sort: "id", SortSafelist: []string{"id"},
	})
	assert.NilError(t, err)

	assert.Equal(t, len(movies), 0)
	assert.DeepEqual(t, metadata, Metadata{})
}
