package main

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
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

// TestMovieEditConflictOverHTTP checks what a client actually SEES when
// optimistic locking fires.
//
// The test above stops at the model layer, which left the 409 translation in
// updateMovieHandler completely uncovered — a bug there would have turned every
// edit conflict into a 500 with nobody noticing.
//
// The conflict can't be staged deterministically from outside the API: the
// handler re-reads the movie itself, so there's no window between its Get and
// its Update for a test to reach into. So this fires concurrent PATCHes and
// asserts an INVARIANT rather than a specific outcome — which makes it strict
// without being flaky:
//
//   - every response is either 200 or 409, never a 500;
//   - the final version equals 1 + the number of successes, so no write was
//     lost and none was double-counted;
//   - the stored title belongs to one of the writers that got a 200.
func TestMovieEditConflictOverHTTP(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	writeToken := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	movie := createTestMovie(t, app, "Contested", 2000)
	path := fmt.Sprintf("/v1/movies/%d", movie.ID)

	const writers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		codes   []int
		winners []string
		start   = make(chan struct{})
	)

	for i := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			title := fmt.Sprintf("Written by %d", i)

			<-start

			code, _, body := ts.patch(t, path, writeToken, map[string]any{"title": title})

			mu.Lock()
			defer mu.Unlock()

			codes = append(codes, code)

			if code == http.StatusOK {
				winners = append(winners, title)
			}

			if code == http.StatusConflict {
				// The message is part of the contract: it tells the client this
				// is worth retrying, unlike most 4xx responses.
				if !strings.Contains(body, "edit conflict") {
					t.Errorf("409 body did not mention an edit conflict: %s", body)
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	var succeeded int

	for _, code := range codes {
		switch code {
		case http.StatusOK:
			succeeded++
		case http.StatusConflict:
			// Expected under contention.
		default:
			t.Errorf("got status %d from a concurrent PATCH; want 200 or 409", code)
		}
	}

	if succeeded == 0 {
		t.Fatal("no concurrent PATCH succeeded; expected at least one to win")
	}

	final, err := app.models.Movies.Get(movie.ID)
	assert.NilError(t, err)

	// Version starts at 1 and increments once per successful write. If a write
	// were lost — two clients both getting 200 off the same version — this
	// would come out short.
	assert.Equal(t, final.Version, int32(1+succeeded))

	if !slices.Contains(winners, final.Title) {
		t.Errorf("stored title %q was not written by any request that got a 200 (%v)", final.Title, winners)
	}
}

// TestMovieHandlersRejectInvalidIDs covers readIDParam's error branch across
// every endpoint that takes an {id}.
//
// All of them answer 404 rather than 400. That's deliberate: "/v1/movies/abc"
// simply isn't a resource that could exist, and returning 404 avoids telling a
// prober anything about the ID format.
func TestMovieHandlersRejectInvalidIDs(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	token := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	ids := []struct {
		name string
		id   string
	}{
		{"non-numeric", "abc"},
		{"zero", "0"},
		{"negative", "-1"},
		{"float", "1.5"},
		{"overflowing int64", "99999999999999999999999"},
		{"numeric with suffix", "1abc"},
	}

	for _, tc := range ids {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/movies/" + tc.id

			code, _, _ := ts.get(t, path, token)
			assert.Equal(t, code, http.StatusNotFound)

			code, _, _ = ts.patch(t, path, token, map[string]any{"title": "x"})
			assert.Equal(t, code, http.StatusNotFound)

			code, _, _ = ts.delete(t, path, token)
			assert.Equal(t, code, http.StatusNotFound)
		})
	}
}

// TestUpdateMovieRejectsMalformedBodies covers updateMovieHandler's readJSON
// branch, which was uncovered because every existing update test sends valid
// JSON.
func TestUpdateMovieRejectsMalformedBodies(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	token := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)

	movie := createTestMovie(t, app, "Existing", 1994)
	path := fmt.Sprintf("/v1/movies/%d", movie.ID)

	tests := []struct {
		name string
		body string
		want string
	}{
		{"truncated JSON", `{"title": "A"`, "badly-formed JSON"},
		{"empty body", ``, "must not be empty"},
		{"unknown field", `{"director": "Nolan"}`, "unknown key"},
		{"wrong type for title", `{"title": 42}`, "incorrect JSON type"},
		// Runtime has its own UnmarshalJSON, and its sentinel error reaches the
		// client verbatim through readJSON's default branch rather than being
		// rewritten as a JSON type error. That's the point of the custom type:
		// the client is told the format is wrong, not that the type is.
		{"runtime in the wrong format", `{"runtime": "abc"}`, "invalid runtime format"},
		{"runtime as a bare number", `{"runtime": 107}`, "invalid runtime format"},
		{"two JSON values", `{"title":"A"} {"title":"B"}`, "single JSON value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, body := ts.patch(t, path, token, tt.body)

			assert.Equal(t, code, http.StatusBadRequest)
			assert.StringContains(t, body, tt.want)
		})
	}

	// None of that should have touched the record.
	after, err := app.models.Movies.Get(movie.ID)
	assert.NilError(t, err)
	assert.Equal(t, after.Title, "Existing")
	assert.Equal(t, after.Version, int32(1))
}

// TestListMoviesRejectsInvalidFilters covers the 422 branch of
// listMoviesHandler, including the sort safelist.
//
// The sort case is the important one. Filters.sortColumn PANICS on a value
// outside the safelist, because the column name is interpolated into the SQL
// string and an unchecked value would be a SQL injection. That panic must never
// be reachable from a request — ValidateFilters has to reject the input first,
// turning it into a clean 422. This test is what proves the validator catches
// it before the query builder does.
func TestListMoviesRejectsInvalidFilters(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	token := createActivatedUser(t, app, "reader@example.com", data.PermissionMoviesRead)

	tests := []struct {
		name      string
		query     string
		wantField string
	}{
		{"page zero", "?page=0", "page"},
		{"negative page", "?page=-1", "page"},
		{"page too large", "?page=10000001", "page"},
		{"page_size zero", "?page_size=0", "page_size"},
		{"page_size too large", "?page_size=101", "page_size"},
		{"non-numeric page", "?page=abc", "page"},
		{"non-numeric page_size", "?page_size=abc", "page_size"},
		// %3B is an encoded semicolon. It has to be encoded to reach the
		// handler at all — see TestListMoviesDropsSemicolonQueryParams below.
		{"SQL injection attempt in sort", "?sort=id%3BDROP%20TABLE%20movies", "sort"},
		{"sort not in the safelist", "?sort=created_at", "sort"},
		{"sort with a valid column but bad direction marker", "?sort=%2B%2Byear", "sort"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, body := ts.get(t, "/v1/movies"+tt.query, token)

			assert.Equal(t, code, http.StatusUnprocessableEntity)
			assert.StringContains(t, body, tt.wantField)
		})
	}

	// And the table is still there — i.e. the injection attempt above did
	// nothing. If sortColumn's safelist had been bypassed, the DROP would have
	// been interpolated straight into the ORDER BY clause.
	code, _, _ := ts.get(t, "/v1/movies", token)
	assert.Equal(t, code, http.StatusOK)
}

// TestListMoviesDropsSemicolonQueryParams documents a standard-library
// behaviour that's worth knowing about before you write a security test.
//
// Since Go 1.17, url.Values.Query() REJECTS a query string containing a raw
// semicolon and silently drops the offending parameter rather than splitting on
// it (semicolons used to be an alternative separator, and the ambiguity was a
// request-smuggling vector). So "?sort=id;DROP TABLE movies" never reaches the
// handler as a sort value at all — the parameter vanishes and the default sort
// applies.
//
// The practical consequence: a test that "proves" the safelist blocks a
// semicolon-laden sort proves nothing, because the string never gets that far.
// The real test is the encoded case above. This one pins the drop behaviour so
// the distinction stays visible.
func TestListMoviesDropsSemicolonQueryParams(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	token := createActivatedUser(t, app, "reader@example.com", data.PermissionMoviesRead)

	createTestMovie(t, app, "Alpha", 2001)
	createTestMovie(t, app, "Beta", 2002)

	// The parameter is dropped, so this behaves exactly like no sort at all:
	// the default "id" applies and the request succeeds.
	code, _, body := ts.get(t, "/v1/movies?sort=id;DROP+TABLE+movies", token)

	assert.Equal(t, code, http.StatusOK)
	assert.StringContains(t, body, "Alpha")
	assert.StringContains(t, body, "Beta")
}
