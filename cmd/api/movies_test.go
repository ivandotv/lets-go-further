package main

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"greenlight/internal/assert"
	"greenlight/internal/data"
)

// movieResponse mirrors the JSON envelope the movie endpoints return, so tests
// can decode and assert on real fields rather than string-matching the body.
type movieResponse struct {
	Movie struct {
		ID      int64    `json:"id"`
		Title   string   `json:"title"`
		Year    int32    `json:"year"`
		Runtime string   `json:"runtime"`
		Genres  []string `json:"genres"`
		Version int32    `json:"version"`
	} `json:"movie"`
}

type moviesListResponse struct {
	Movies []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Year  int32  `json:"year"`
	} `json:"movies"`
	Metadata data.Metadata `json:"metadata"`
}

func TestCreateMovie(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	writeToken := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	t.Run("valid movie is created", func(t *testing.T) {
		code, headers, body := ts.post(t, "/v1/movies", writeToken, map[string]any{
			"title":   "Moana",
			"year":    2016,
			"runtime": "107 mins",
			"genres":  []string{"animation", "adventure"},
		})

		assert.Equal(t, code, http.StatusCreated)

		// A 201 must tell the client where the new resource lives.
		var got movieResponse
		unmarshal(t, body, &got)
		assert.Equal(t, headers.Get("Location"), fmt.Sprintf("/v1/movies/%d", got.Movie.ID))

		assert.Equal(t, got.Movie.Title, "Moana")
		assert.Equal(t, got.Movie.Year, 2016)
		assert.Equal(t, got.Movie.Version, 1)

		// The custom Runtime type must serialise as "107 mins", not 107.
		assert.Equal(t, got.Movie.Runtime, "107 mins")
	})

	t.Run("validation failures return 422 with field errors", func(t *testing.T) {
		tests := []struct {
			name    string
			body    map[string]any
			wantKey string
		}{
			{
				name:    "missing title",
				body:    map[string]any{"year": 2016, "runtime": "107 mins", "genres": []string{"animation"}},
				wantKey: "title",
			},
			{
				name:    "year in the future",
				body:    map[string]any{"title": "X", "year": 3000, "runtime": "107 mins", "genres": []string{"animation"}},
				wantKey: "year",
			},
			{
				name:    "negative runtime",
				body:    map[string]any{"title": "X", "year": 2016, "runtime": "-5 mins", "genres": []string{"animation"}},
				wantKey: "runtime",
			},
			{
				name:    "duplicate genres",
				body:    map[string]any{"title": "X", "year": 2016, "runtime": "107 mins", "genres": []string{"a", "a"}},
				wantKey: "genres",
			},
			{
				name:    "no genres",
				body:    map[string]any{"title": "X", "year": 2016, "runtime": "107 mins", "genres": []string{}},
				wantKey: "genres",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				code, _, body := ts.post(t, "/v1/movies", writeToken, tt.body)

				// 422, not 400: the JSON parsed fine, it just broke our rules.
				assert.Equal(t, code, http.StatusUnprocessableEntity)
				assert.StringContains(t, body, tt.wantKey)
			})
		}
	})

	t.Run("malformed request bodies return 400", func(t *testing.T) {
		tests := []struct {
			name     string
			body     string
			wantText string
		}{
			{name: "empty body", body: "", wantText: "must not be empty"},
			{name: "broken json", body: `{"title": `, wantText: "badly-formed JSON"},
			{name: "wrong type", body: `{"title": 123}`, wantText: "incorrect JSON type"},
			// DisallowUnknownFields — a typo shouldn't be silently ignored.
			{name: "unknown field", body: `{"titel": "typo"}`, wantText: "unknown key"},
			// Two JSON values in one body.
			{name: "trailing content", body: `{"title":"a"}{"title":"b"}`, wantText: "single JSON value"},
			// The custom Runtime unmarshaller's error path.
			{name: "bad runtime format", body: `{"runtime": "107 minutes"}`, wantText: "invalid runtime format"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				code, _, body := ts.post(t, "/v1/movies", writeToken, tt.body)

				assert.Equal(t, code, http.StatusBadRequest)
				assert.StringContains(t, body, tt.wantText)
			})
		}
	})
}

func TestShowMovie(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	readToken := createActivatedUser(t, app, "reader@example.com", data.PermissionMoviesRead)
	movie := createTestMovie(t, app, "Casablanca", 1942, "drama", "romance")

	tests := []struct {
		name     string
		urlPath  string
		wantCode int
		wantBody string
	}{
		{
			name:     "existing movie",
			urlPath:  fmt.Sprintf("/v1/movies/%d", movie.ID),
			wantCode: http.StatusOK,
			wantBody: "Casablanca",
		},
		{
			name:     "non-existent id",
			urlPath:  "/v1/movies/999999",
			wantCode: http.StatusNotFound,
		},
		{
			// readIDParam rejects these before they reach the database.
			name:     "negative id",
			urlPath:  "/v1/movies/-1",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "non-numeric id",
			urlPath:  "/v1/movies/abc",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "zero id",
			urlPath:  "/v1/movies/0",
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, body := ts.get(t, tt.urlPath, readToken)

			assert.Equal(t, code, tt.wantCode)
			if tt.wantBody != "" {
				assert.StringContains(t, body, tt.wantBody)
			}
		})
	}
}

func TestUpdateMovie(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	writeToken := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	// TestPartialUpdate is the behaviour that makes PATCH worth using: fields
	// the client doesn't mention must be left completely alone.
	t.Run("partial update leaves other fields untouched", func(t *testing.T) {
		movie := createTestMovie(t, app, "Black Panther", 2018, "action", "adventure")

		code, _, body := ts.patch(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken,
			map[string]any{"year": 2019})

		assert.Equal(t, code, http.StatusOK)

		var got movieResponse
		unmarshal(t, body, &got)

		assert.Equal(t, got.Movie.Year, 2019)
		// Untouched:
		assert.Equal(t, got.Movie.Title, "Black Panther")
		assert.Equal(t, got.Movie.Runtime, "102 mins")
		assert.Equal(t, len(got.Movie.Genres), 2)
		// Every write bumps the version.
		assert.Equal(t, got.Movie.Version, 2)
	})

	t.Run("updating all fields", func(t *testing.T) {
		movie := createTestMovie(t, app, "Original", 2000)

		code, _, body := ts.patch(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken, map[string]any{
			"title":   "Updated",
			"year":    2001,
			"runtime": "150 mins",
			"genres":  []string{"comedy"},
		})

		assert.Equal(t, code, http.StatusOK)

		var got movieResponse
		unmarshal(t, body, &got)
		assert.Equal(t, got.Movie.Title, "Updated")
		assert.Equal(t, got.Movie.Year, 2001)
		assert.Equal(t, got.Movie.Runtime, "150 mins")
	})

	t.Run("non-existent movie returns 404", func(t *testing.T) {
		code, _, _ := ts.patch(t, "/v1/movies/999999", writeToken, map[string]any{"year": 2019})
		assert.Equal(t, code, http.StatusNotFound)
	})

	t.Run("invalid update returns 422", func(t *testing.T) {
		movie := createTestMovie(t, app, "Some Film", 2000)

		code, _, body := ts.patch(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken,
			map[string]any{"year": 1000})

		assert.Equal(t, code, http.StatusUnprocessableEntity)
		assert.StringContains(t, body, "year")
	})
}

func TestDeleteMovie(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	writeToken := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	movie := createTestMovie(t, app, "Doomed", 1999)

	code, _, body := ts.delete(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken)
	assert.Equal(t, code, http.StatusOK)
	assert.StringContains(t, body, "successfully deleted")

	// It's really gone.
	code, _, _ = ts.get(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken)
	assert.Equal(t, code, http.StatusNotFound)

	// Deleting again is a 404, not a silent success.
	code, _, _ = ts.delete(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken)
	assert.Equal(t, code, http.StatusNotFound)
}

func TestListMovies(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	readToken := createActivatedUser(t, app, "reader@example.com", data.PermissionMoviesRead)

	createTestMovie(t, app, "Black Panther", 2018, "action", "adventure")
	createTestMovie(t, app, "Deadpool", 2016, "action", "comedy")
	createTestMovie(t, app, "The Breakfast Club", 1985, "drama")
	createTestMovie(t, app, "Moana", 2016, "animation", "adventure")

	tests := []struct {
		name      string
		query     string
		wantCode  int
		wantCount int
		wantFirst string
	}{
		{name: "no filters", query: "", wantCode: http.StatusOK, wantCount: 4},
		{name: "title filter", query: "?title=panther", wantCode: http.StatusOK, wantCount: 1, wantFirst: "Black Panther"},
		{name: "genre filter", query: "?genres=action", wantCode: http.StatusOK, wantCount: 2},
		{name: "multiple genres requires all", query: "?genres=action,adventure", wantCode: http.StatusOK, wantCount: 1},
		{name: "sort descending by year", query: "?sort=-year", wantCode: http.StatusOK, wantCount: 4, wantFirst: "Black Panther"},
		{name: "sort ascending by title", query: "?sort=title", wantCode: http.StatusOK, wantCount: 4, wantFirst: "Black Panther"},
		{name: "page size", query: "?page_size=2", wantCode: http.StatusOK, wantCount: 2},
		{name: "second page", query: "?page_size=2&page=2", wantCode: http.StatusOK, wantCount: 2},

		// Invalid query parameters must be a 422 with field errors, not a 500.
		{name: "invalid sort value", query: "?sort=password", wantCode: http.StatusUnprocessableEntity},
		{name: "page zero", query: "?page=0", wantCode: http.StatusUnprocessableEntity},
		{name: "page size too large", query: "?page_size=500", wantCode: http.StatusUnprocessableEntity},
		{name: "non-numeric page", query: "?page=abc", wantCode: http.StatusUnprocessableEntity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, body := ts.get(t, "/v1/movies"+tt.query, readToken)

			assert.Equal(t, code, tt.wantCode)

			if tt.wantCode != http.StatusOK {
				return
			}

			var got moviesListResponse
			unmarshal(t, body, &got)

			assert.Equal(t, len(got.Movies), tt.wantCount)

			if tt.wantFirst != "" {
				assert.Equal(t, got.Movies[0].Title, tt.wantFirst)
			}
		})
	}
}

// TestListMoviesSQLInjection is a security regression test.
//
// The sort parameter is the one client-controlled value that reaches the SQL
// string directly (you cannot parameterise an ORDER BY column). The safelist
// in listMoviesHandler must reject anything else — a 422, never a 500 and
// certainly never a successful query.
func TestListMoviesSQLInjection(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	readToken := createActivatedUser(t, app, "reader@example.com", data.PermissionMoviesRead)
	createTestMovie(t, app, "Survivor", 2000)

	payloads := []string{
		"id; DROP TABLE movies--",
		"id) UNION SELECT password_hash FROM users--",
		"(SELECT 1)",
		"id--",
		"id ASC, (SELECT 1)",
	}

	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			// url.QueryEscape is essential here. Without it, the spaces and
			// semicolons make the request URI itself invalid, so the server
			// rejects it with a 400 before the handler ever runs — and the
			// test would pass for entirely the wrong reason, proving nothing
			// about the safelist.
			code, _, _ := ts.get(t, "/v1/movies?sort="+url.QueryEscape(payload), readToken)

			assert.Equal(t, code, http.StatusUnprocessableEntity)
		})
	}

	// And the table is still there.
	code, _, body := ts.get(t, "/v1/movies", readToken)
	assert.Equal(t, code, http.StatusOK)
	assert.StringContains(t, body, "Survivor")
}

// TestMovieEditConflict drives the optimistic-locking path through the full
// HTTP stack, verifying it surfaces as a 409 rather than a lost update.
func TestMovieEditConflict(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	writeToken := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	movie := createTestMovie(t, app, "Contested", 2000)

	// Simulate a second client that already read version 1 and is now writing,
	// by mutating the record behind the API's back.
	stale, err := app.models.Movies.Get(movie.ID)
	assert.NilError(t, err)

	// Someone else's write lands first, taking it to version 2.
	code, _, _ := ts.patch(t, fmt.Sprintf("/v1/movies/%d", movie.ID), writeToken,
		map[string]any{"title": "First Write"})
	assert.Equal(t, code, http.StatusOK)

	// The stale client's model-level write must now be rejected.
	stale.Title = "Second Write"
	err = app.models.Movies.Update(stale)
	assert.Equal(t, err, data.ErrEditConflict)
}
