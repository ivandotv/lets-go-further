package data

import (
	"slices"
	"testing"
	"time"

	"greenlight/internal/assert"
	"greenlight/internal/validator"
)

// newTestMovie is a small factory so each test can create a movie without
// repeating the boilerplate. Tests that care about specific values override
// them after the call.
func newTestMovie(t *testing.T, m MovieModel, title string, year int32, genres ...string) *Movie {
	t.Helper()

	if len(genres) == 0 {
		genres = []string{"drama"}
	}

	movie := &Movie{
		Title:   title,
		Year:    year,
		Runtime: 102,
		Genres:  Genres(genres),
	}

	assert.NilError(t, m.Insert(movie))

	return movie
}

func TestMovieModel_InsertAndGet(t *testing.T) {
	models := newTestModels(t)

	movie := &Movie{
		Title:   "Casablanca",
		Year:    1942,
		Runtime: 102,
		Genres:  Genres{"drama", "romance"},
	}

	err := models.Movies.Insert(movie)
	assert.NilError(t, err)

	// Insert must back-fill the database-generated fields onto the struct we
	// passed in. This is what lets the handler return a complete record
	// (including its Location header) without a follow-up query.
	if movie.ID < 1 {
		t.Errorf("expected Insert to set an ID; got %d", movie.ID)
	}
	assert.Equal(t, movie.Version, 1)
	if movie.CreatedAt.IsZero() {
		t.Error("expected Insert to set CreatedAt")
	}

	// Now read it back and check every field survived the round trip —
	// especially Genres, which goes through our custom Valuer/Scanner, and
	// CreatedAt, which relies on the driver's TIMESTAMP handling.
	got, err := models.Movies.Get(movie.ID)
	assert.NilError(t, err)

	assert.Equal(t, got.ID, movie.ID)
	assert.Equal(t, got.Title, "Casablanca")
	assert.Equal(t, got.Year, 1942)
	assert.Equal(t, got.Runtime, Runtime(102))
	assert.Equal(t, got.Version, 1)

	if !slices.Equal(got.Genres, Genres{"drama", "romance"}) {
		t.Errorf("got genres %#v; want [drama romance]", got.Genres)
	}

	// Times don't survive == comparison reliably (monotonic clock, location),
	// so compare with Equal and a tolerance.
	if got.CreatedAt.Sub(movie.CreatedAt).Abs() > time.Second {
		t.Errorf("got CreatedAt %v; want ~%v", got.CreatedAt, movie.CreatedAt)
	}
}

func TestMovieModel_GetNotFound(t *testing.T) {
	models := newTestModels(t)

	tests := []struct {
		name string
		id   int64
	}{
		{name: "no such record", id: 999},
		// IDs below 1 are short-circuited before hitting the database, but
		// must produce the same error so the handler's 404 path is identical.
		{name: "zero id", id: 0},
		{name: "negative id", id: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := models.Movies.Get(tt.id)

			assert.ErrorIs(t, err, ErrRecordNotFound)
		})
	}
}

func TestMovieModel_Update(t *testing.T) {
	models := newTestModels(t)

	movie := newTestMovie(t, models.Movies, "Black Panther", 2018, "action")

	movie.Title = "Black Panther: Wakanda Forever"
	movie.Year = 2022
	movie.Genres = Genres{"action", "adventure"}

	err := models.Movies.Update(movie)
	assert.NilError(t, err)

	// Every successful update bumps the version — that's what makes the
	// optimistic-locking check work.
	assert.Equal(t, movie.Version, 2)

	got, err := models.Movies.Get(movie.ID)
	assert.NilError(t, err)
	assert.Equal(t, got.Title, "Black Panther: Wakanda Forever")
	assert.Equal(t, got.Year, 2022)
	assert.Equal(t, got.Version, 2)
	if !slices.Equal(got.Genres, Genres{"action", "adventure"}) {
		t.Errorf("got genres %#v; want [action adventure]", got.Genres)
	}
}

// TestMovieModel_UpdateEditConflict is the important one: it simulates two
// clients racing to update the same record, and verifies the second one is
// rejected instead of silently clobbering the first.
//
// This is the "lost update" problem, and it's the reason the version column
// exists at all.
func TestMovieModel_UpdateEditConflict(t *testing.T) {
	models := newTestModels(t)

	movie := newTestMovie(t, models.Movies, "Deadpool", 2016, "action")

	// Two clients both read the record at version 1.
	clientA, err := models.Movies.Get(movie.ID)
	assert.NilError(t, err)

	clientB, err := models.Movies.Get(movie.ID)
	assert.NilError(t, err)

	// Client A writes first and succeeds, taking the record to version 2.
	clientA.Title = "Deadpool (A's edit)"
	assert.NilError(t, models.Movies.Update(clientA))

	// Client B now writes based on its stale version 1. Its WHERE clause
	// matches no rows, so it must be rejected.
	clientB.Title = "Deadpool (B's edit)"
	err = models.Movies.Update(clientB)

	assert.ErrorIs(t, err, ErrEditConflict)

	// And A's write must have survived intact.
	got, err := models.Movies.Get(movie.ID)
	assert.NilError(t, err)
	assert.Equal(t, got.Title, "Deadpool (A's edit)")
	assert.Equal(t, got.Version, 2)
}

func TestMovieModel_Delete(t *testing.T) {
	models := newTestModels(t)

	movie := newTestMovie(t, models.Movies, "The Breakfast Club", 1985)

	assert.NilError(t, models.Movies.Delete(movie.ID))

	// It should be gone.
	_, err := models.Movies.Get(movie.ID)
	assert.ErrorIs(t, err, ErrRecordNotFound)

	// Deleting again must report not-found rather than succeeding silently.
	// SQL happily deletes zero rows, so this behaviour only exists because
	// Delete checks RowsAffected.
	err = models.Movies.Delete(movie.ID)
	assert.ErrorIs(t, err, ErrRecordNotFound)
}

// seedMovies inserts a fixed set of records for the GetAll tests.
func seedMovies(t *testing.T, m MovieModel) {
	t.Helper()

	newTestMovie(t, m, "Black Panther", 2018, "action", "adventure")
	newTestMovie(t, m, "Deadpool", 2016, "action", "comedy")
	newTestMovie(t, m, "The Breakfast Club", 1985, "drama")
	newTestMovie(t, m, "Moana", 2016, "animation", "adventure")
	newTestMovie(t, m, "Casablanca", 1942, "drama", "romance")
}

func TestMovieModel_GetAllFiltering(t *testing.T) {
	models := newTestModels(t)
	seedMovies(t, models.Movies)

	baseFilters := Filters{
		Page:         1,
		PageSize:     20,
		Sort:         "id",
		SortSafelist: testSafelist,
	}

	tests := []struct {
		name       string
		title      string
		genres     []string
		wantTitles []string
	}{
		{
			name:       "no filters returns everything",
			wantTitles: []string{"Black Panther", "Deadpool", "The Breakfast Club", "Moana", "Casablanca"},
		},
		{
			name:       "partial title match",
			title:      "panther",
			wantTitles: []string{"Black Panther"},
		},
		{
			// SQLite's LIKE is case-insensitive for ASCII by default, which
			// gives us the same behaviour as the book's full-text search
			// without any extra work.
			name:       "title match is case insensitive",
			title:      "BREAKFAST",
			wantTitles: []string{"The Breakfast Club"},
		},
		{
			name:       "title matching nothing",
			title:      "zzzzz",
			wantTitles: []string{},
		},
		{
			name:       "single genre",
			genres:     []string{"action"},
			wantTitles: []string{"Black Panther", "Deadpool"},
		},
		{
			// This is the containment case: a movie must have ALL the
			// requested genres, not just one of them.
			name:       "multiple genres requires all of them",
			genres:     []string{"action", "adventure"},
			wantTitles: []string{"Black Panther"},
		},
		{
			name:       "genre matching nothing",
			genres:     []string{"documentary"},
			wantTitles: []string{},
		},
		{
			name:       "title and genre combined",
			title:      "a",
			genres:     []string{"drama"},
			wantTitles: []string{"The Breakfast Club", "Casablanca"},
		},
		{
			name:       "empty genres slice is not a filter",
			genres:     []string{},
			wantTitles: []string{"Black Panther", "Deadpool", "The Breakfast Club", "Moana", "Casablanca"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			movies, metadata, err := models.Movies.GetAll(tt.title, tt.genres, baseFilters)
			assert.NilError(t, err)

			gotTitles := make([]string, len(movies))
			for i, movie := range movies {
				gotTitles[i] = movie.Title
			}

			if !slices.Equal(gotTitles, tt.wantTitles) {
				t.Errorf("got titles %v; want %v", gotTitles, tt.wantTitles)
			}

			// The metadata's total must agree with the number of rows, which
			// is what verifies the `count(*) OVER()` window function.
			assert.Equal(t, metadata.TotalRecords, len(tt.wantTitles))
		})
	}
}

func TestMovieModel_GetAllSorting(t *testing.T) {
	models := newTestModels(t)
	seedMovies(t, models.Movies)

	tests := []struct {
		name       string
		sort       string
		wantTitles []string
	}{
		{
			name: "by year ascending",
			sort: "year",
			wantTitles: []string{
				"Casablanca", "The Breakfast Club", "Deadpool", "Moana", "Black Panther",
			},
		},
		{
			name: "by year descending",
			sort: "-year",
			wantTitles: []string{
				"Black Panther", "Deadpool", "Moana", "The Breakfast Club", "Casablanca",
			},
		},
		{
			name: "by title ascending",
			sort: "title",
			wantTitles: []string{
				"Black Panther", "Casablanca", "Deadpool", "Moana", "The Breakfast Club",
			},
		},
	}

	// Note the year-sorted cases: Deadpool and Moana are both from 2016. The
	// secondary `id ASC` tiebreaker in the query is what makes their relative
	// order deterministic — without it this test would be flaky.

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters := Filters{Page: 1, PageSize: 20, Sort: tt.sort, SortSafelist: testSafelist}

			movies, _, err := models.Movies.GetAll("", nil, filters)
			assert.NilError(t, err)

			gotTitles := make([]string, len(movies))
			for i, movie := range movies {
				gotTitles[i] = movie.Title
			}

			if !slices.Equal(gotTitles, tt.wantTitles) {
				t.Errorf("got titles %v; want %v", gotTitles, tt.wantTitles)
			}
		})
	}
}

func TestMovieModel_GetAllPagination(t *testing.T) {
	models := newTestModels(t)
	seedMovies(t, models.Movies) // 5 records

	tests := []struct {
		name         string
		page         int
		pageSize     int
		wantCount    int
		wantMetadata Metadata
	}{
		{
			name: "first page of two", page: 1, pageSize: 3, wantCount: 3,
			wantMetadata: Metadata{CurrentPage: 1, PageSize: 3, FirstPage: 1, LastPage: 2, TotalRecords: 5},
		},
		{
			name: "last page is partial", page: 2, pageSize: 3, wantCount: 2,
			wantMetadata: Metadata{CurrentPage: 2, PageSize: 3, FirstPage: 1, LastPage: 2, TotalRecords: 5},
		},
		{
			name: "page beyond the end is empty", page: 3, pageSize: 3, wantCount: 0,
			// No rows come back, so `count(*) OVER()` never executes and
			// totalRecords stays 0 — which means an out-of-range page reports
			// empty metadata. Worth knowing about; the book behaves the same.
			wantMetadata: Metadata{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters := Filters{Page: tt.page, PageSize: tt.pageSize, Sort: "id", SortSafelist: testSafelist}

			movies, metadata, err := models.Movies.GetAll("", nil, filters)
			assert.NilError(t, err)

			assert.Equal(t, len(movies), tt.wantCount)
			assert.Equal(t, metadata, tt.wantMetadata)
		})
	}
}

// TestMovieModel_GetAllReturnsEmptySliceNotNil guards a small but real API
// contract: an empty result must encode as `[]` in JSON, never `null`.
func TestMovieModel_GetAllReturnsEmptySliceNotNil(t *testing.T) {
	models := newTestModels(t)

	filters := Filters{Page: 1, PageSize: 20, Sort: "id", SortSafelist: testSafelist}

	movies, _, err := models.Movies.GetAll("", nil, filters)
	assert.NilError(t, err)

	if movies == nil {
		t.Fatal("got a nil slice; want an empty one (it would serialise as JSON null)")
	}
	assert.Equal(t, len(movies), 0)
}

func TestValidateMovie(t *testing.T) {
	// A movie that passes every rule, which each case then breaks in one way.
	valid := func() *Movie {
		return &Movie{
			Title:   "Casablanca",
			Year:    1942,
			Runtime: 102,
			Genres:  Genres{"drama"},
		}
	}

	tests := []struct {
		name    string
		modify  func(*Movie)
		wantKey string // the field expected to fail, "" means it should pass
	}{
		{name: "valid movie", modify: func(*Movie) {}},
		{name: "empty title", modify: func(m *Movie) { m.Title = "" }, wantKey: "title"},
		{name: "title too long", modify: func(m *Movie) { m.Title = string(make([]byte, 501)) }, wantKey: "title"},
		{name: "missing year", modify: func(m *Movie) { m.Year = 0 }, wantKey: "year"},
		{name: "year before cinema", modify: func(m *Movie) { m.Year = 1800 }, wantKey: "year"},
		{name: "year in the future", modify: func(m *Movie) { m.Year = int32(time.Now().Year() + 1) }, wantKey: "year"},
		{name: "missing runtime", modify: func(m *Movie) { m.Runtime = 0 }, wantKey: "runtime"},
		{name: "negative runtime", modify: func(m *Movie) { m.Runtime = -5 }, wantKey: "runtime"},
		{name: "nil genres", modify: func(m *Movie) { m.Genres = nil }, wantKey: "genres"},
		{name: "empty genres", modify: func(m *Movie) { m.Genres = Genres{} }, wantKey: "genres"},
		{name: "too many genres", modify: func(m *Movie) {
			m.Genres = Genres{"a", "b", "c", "d", "e", "f"}
		}, wantKey: "genres"},
		{name: "duplicate genres", modify: func(m *Movie) { m.Genres = Genres{"drama", "drama"} }, wantKey: "genres"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			movie := valid()
			tt.modify(movie)

			v := validator.New()
			ValidateMovie(v, movie)

			if tt.wantKey == "" {
				if !v.Valid() {
					t.Errorf("expected movie to be valid; got errors: %v", v.Errors)
				}
				return
			}

			if v.Valid() {
				t.Fatalf("expected a validation error on %q, but the movie was valid", tt.wantKey)
			}
			if _, ok := v.Errors[tt.wantKey]; !ok {
				t.Errorf("expected an error on %q; got errors: %v", tt.wantKey, v.Errors)
			}
		})
	}
}
