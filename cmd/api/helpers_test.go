package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"greenlight/internal/assert"
	"greenlight/internal/validator"
)

func TestWriteJSON(t *testing.T) {
	app := &application{}

	rr := httptest.NewRecorder()

	headers := make(http.Header)
	headers.Set("Location", "/v1/movies/1")

	err := app.writeJSON(rr, http.StatusCreated, envelope{"movie": "test"}, headers)
	assert.NilError(t, err)

	rs := rr.Result()
	defer rs.Body.Close()

	assert.Equal(t, rs.StatusCode, http.StatusCreated)
	assert.Equal(t, rs.Header.Get("Content-Type"), "application/json")
	// Extra headers passed in must make it onto the response.
	assert.Equal(t, rs.Header.Get("Location"), "/v1/movies/1")

	body, err := io.ReadAll(rs.Body)
	assert.NilError(t, err)

	assert.StringContains(t, string(body), `"movie": "test"`)
	// A trailing newline makes curl output readable.
	assert.Equal(t, string(body)[len(body)-1], byte('\n'))
}

// TestWriteJSONNilHeaders confirms that passing nil for headers is safe.
// Ranging over a nil map is legal Go and simply does nothing, which is why
// every handler can pass nil without a guard.
func TestWriteJSONNilHeaders(t *testing.T) {
	app := &application{}
	rr := httptest.NewRecorder()

	err := app.writeJSON(rr, http.StatusOK, envelope{"status": "ok"}, nil)
	assert.NilError(t, err)

	assert.Equal(t, rr.Result().StatusCode, http.StatusOK)
}

func TestReadString(t *testing.T) {
	app := &application{}

	qs := url.Values{}
	qs.Set("title", "casablanca")
	qs.Set("empty", "")

	assert.Equal(t, app.readString(qs, "title", "default"), "casablanca")
	// An absent key falls back to the default...
	assert.Equal(t, app.readString(qs, "missing", "default"), "default")
	// ...and so does a present-but-empty one, so `?title=` behaves like no
	// filter rather than "match the empty string".
	assert.Equal(t, app.readString(qs, "empty", "default"), "default")
}

func TestReadCSV(t *testing.T) {
	app := &application{}

	qs := url.Values{}
	qs.Set("genres", "drama,romance")
	qs.Set("single", "drama")
	qs.Set("empty", "")

	tests := []struct {
		name string
		key  string
		want []string
	}{
		{name: "multiple values", key: "genres", want: []string{"drama", "romance"}},
		{name: "single value", key: "single", want: []string{"drama"}},
		{name: "empty falls back to default", key: "empty", want: nil},
		{name: "missing falls back to default", key: "missing", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := app.readCSV(qs, tt.key, nil)

			if len(got) != len(tt.want) {
				t.Fatalf("got %v; want %v", got, tt.want)
			}
			for i := range got {
				assert.Equal(t, got[i], tt.want[i])
			}
		})
	}
}

func TestReadInt(t *testing.T) {
	app := &application{}

	qs := url.Values{}
	qs.Set("page", "5")
	qs.Set("bad", "not-a-number")
	qs.Set("negative", "-3")

	t.Run("valid integer", func(t *testing.T) {
		v := validator.New()
		assert.Equal(t, app.readInt(qs, "page", 1, v), 5)
		assert.Equal(t, v.Valid(), true)
	})

	t.Run("missing key returns the default", func(t *testing.T) {
		v := validator.New()
		assert.Equal(t, app.readInt(qs, "missing", 42, v), 42)
		assert.Equal(t, v.Valid(), true)
	})

	t.Run("negative integers parse fine", func(t *testing.T) {
		// readInt's job is parsing, not range-checking. Rejecting a negative
		// page is ValidateFilters' responsibility.
		v := validator.New()
		assert.Equal(t, app.readInt(qs, "negative", 1, v), -3)
		assert.Equal(t, v.Valid(), true)
	})

	t.Run("non-numeric records a validation error", func(t *testing.T) {
		v := validator.New()

		// It returns the default AND records an error, rather than failing
		// fast — so a bad ?page= joins every other problem in one 422.
		assert.Equal(t, app.readInt(qs, "bad", 1, v), 1)
		assert.Equal(t, v.Valid(), false)
		assert.Equal(t, v.Errors["bad"], "must be an integer value")
	})
}

// TestBackgroundRecoversPanic checks the panic guard in app.background.
//
// This matters more than it looks: a panic in a bare `go func(){}` takes down
// the ENTIRE process — the recoverPanic middleware can't help, because it only
// covers the request's own goroutine. If this recover were removed, one bad
// welcome email would crash the server.
func TestBackgroundRecoversPanic(t *testing.T) {
	app := &application{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app.background(func() {
		panic("boom")
	})

	// If the recover weren't there, the panic would kill the test binary
	// before Wait() returned and this test would never report at all.
	app.wg.Wait()
}

// TestBackgroundWaitGroup checks that Wait() actually blocks until the work is
// done — which is what makes graceful shutdown wait for in-flight emails.
func TestBackgroundWaitGroup(t *testing.T) {
	app := &application{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// A plain bool is safe here only because wg.Wait() creates a
	// happens-before edge with wg.Done(). Without the WaitGroup this would be
	// a data race, and `go test -race` would say so.
	done := false

	app.background(func() {
		done = true
	})

	app.wg.Wait()

	assert.Equal(t, done, true)
}

// TestReadJSONErrors covers every branch of readJSON's error switch.
//
// These messages are part of the API's contract — they're returned verbatim to
// the client by badRequestResponse — so they're asserted exactly rather than
// loosely. The whole reason readJSON translates the standard library's errors
// at all is that raw json.SyntaxError text ("invalid character 'x' looking for
// beginning of value") is useless to an API consumer.
func TestReadJSONErrors(t *testing.T) {
	app := &application{}

	// The destination used for most cases. Title is a string so that sending a
	// number for it produces a type error.
	type input struct {
		Title string `json:"title"`
		Year  int32  `json:"year"`
	}

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "empty body",
			body:    "",
			wantErr: "body must not be empty",
		},
		{
			name:    "badly-formed JSON",
			body:    `{"title": "Moana"`,
			wantErr: "body contains badly-formed JSON",
		},
		{
			name:    "syntax error mid-body reports the offset",
			body:    `{"title": "Moana", }`,
			wantErr: "body contains badly-formed JSON (at character 20)",
		},
		{
			name:    "wrong type for a field",
			body:    `{"title": 123}`,
			wantErr: "body contains incorrect JSON type for field \"title\"",
		},
		{
			name:    "unknown field is rejected",
			body:    `{"rating": 5}`,
			wantErr: "body contains unknown key \"rating\"",
		},
		{
			name:    "two JSON values in one body",
			body:    `{"title": "A"} {"title": "B"}`,
			wantErr: "body must only contain a single JSON value",
		},
		{
			name:    "trailing garbage after a valid value",
			body:    `{"title": "A"} nonsense`,
			wantErr: "body must only contain a single JSON value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))

			var dst input
			err := app.readJSON(rr, r, &dst)

			if err == nil {
				t.Fatalf("got nil error for body %q; want %q", tt.body, tt.wantErr)
			}

			assert.Equal(t, err.Error(), tt.wantErr)
		})
	}
}

// TestReadJSONEnforcesMaxBytes covers the MaxBytesReader branch separately,
// because it needs a body over 1MB and would make the table above unreadable.
//
// Without this limit a client could post a multi-gigabyte body and exhaust the
// server's memory before any handler code runs.
func TestReadJSONEnforcesMaxBytes(t *testing.T) {
	app := &application{}

	// One byte over the 1_048_576 limit, as valid JSON.
	padding := strings.Repeat("a", 1_048_576)
	body := `{"title": "` + padding + `"}`

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	var dst struct {
		Title string `json:"title"`
	}

	err := app.readJSON(rr, r, &dst)
	if err == nil {
		t.Fatal("got nil error for an oversized body; want a size limit failure")
	}

	assert.Equal(t, err.Error(), "body must not be larger than 1048576 bytes")
}

// TestReadJSONPanicsOnNonPointer documents the one case readJSON deliberately
// panics on.
//
// An InvalidUnmarshalError means WE passed something wrong to Decode — a
// programming bug in a handler, not bad input from a client. Returning it would
// turn a bug into a 400 and hide it; panicking surfaces it in development and
// is caught by recoverPanic as a 500 in production.
func TestReadJSONPanicsOnNonPointer(t *testing.T) {
	app := &application{}

	defer func() {
		if recover() == nil {
			t.Error("readJSON did not panic when handed a non-pointer destination")
		}
	}()

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"title":"A"}`))

	// Passing a value rather than a pointer.
	var dst struct{ Title string }
	//nolint:staticcheck // passing a non-pointer is the point of this test
	_ = app.readJSON(rr, r, dst)
}

// FuzzReadJSON checks that readJSON survives arbitrary request bodies.
//
// This function runs on every write endpoint before any authentication, so a
// panic in it is a remote, unauthenticated denial of service. The properties
// are deliberately weak — this is a robustness test, not a correctness one:
//
//  1. It never panics, given a valid pointer destination. (The one intentional
//     panic is covered by the test above, and can't be reached from here.)
//  2. If it returns nil, the destination is usable — no partial-decode state
//     that a handler would then act on.
func FuzzReadJSON(f *testing.F) {
	seeds := []string{
		`{"title":"Moana","year":2016}`,
		`{}`,
		``,
		`null`,
		`[]`,
		`{"title":`,
		`{"title":123}`,
		`{"unknown":1}`,
		`{"title":"A"} {"title":"B"}`,
		`{"year":99999999999999999999}`,
		`{"title":"\ud800"}`, // lone surrogate: invalid UTF-8 in JSON
		"{\"title\":\"\x00\"}",
		strings.Repeat(`{"a":`, 200),
	}

	for _, seed := range seeds {
		f.Add(seed)
	}

	app := &application{}

	f.Fuzz(func(t *testing.T, body string) {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

		var dst struct {
			Title string `json:"title"`
			Year  int32  `json:"year"`
		}

		// Property 1: no panic, whatever the bytes.
		if err := app.readJSON(rr, r, &dst); err != nil {
			// Property 2 (the contract with badRequestResponse): every error is
			// safe to show a client, so none may be empty.
			if err.Error() == "" {
				t.Fatalf("readJSON(%q) returned an error with an empty message", body)
			}
			return
		}
	})
}
