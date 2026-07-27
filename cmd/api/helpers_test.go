package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
