package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"greenlight/internal/data"
)

// ─────────────────────────────────────────────────────────────────────────────
// BENCHMARKS FOR THE HTTP LAYER
// ─────────────────────────────────────────────────────────────────────────────
//
//	go test ./cmd/api/ -bench=. -benchmem -run=^$
//
// Reading that command: -bench=. runs every benchmark ("." is a regexp matching
// all names), -benchmem adds the allocation columns, and -run=^$ matches no
// ordinary test, so nothing but the benchmarks runs.
//
// The output looks like:
//
//	BenchmarkWriteJSON-8    200    8759 ns/op    2107 B/op    18 allocs/op
//	                    │     │         │             │            └ allocations per call
//	                    │     │         │             └ bytes allocated per call
//	                    │     │         └ nanoseconds per call
//	                    │     └ how many iterations it chose to run
//	                    └ GOMAXPROCS (CPU count)
//
// allocs/op is usually the number to watch: allocations create garbage-collector
// work, and they're a more stable signal than ns/op, which moves around with
// whatever else your machine is doing.
//
// These use `for b.Loop()` (Go 1.24+) rather than the older
// `for i := 0; i < b.N; i++`. See internal/data/bench_test.go for why that
// difference matters beyond style.

// BenchmarkWriteJSON measures the response encoder.
//
// It runs on every single response, and it uses json.MarshalIndent rather than
// Marshal — a deliberate choice for human-readable curl output that costs both
// time and allocations. This is where you'd measure whether that trade is still
// one you want to make.
func BenchmarkWriteJSON(b *testing.B) {
	app := &application{}

	movie := &data.Movie{
		ID:      1,
		Title:   "Everything Everywhere All at Once",
		Year:    2022,
		Runtime: 139,
		Genres:  data.Genres{"action", "comedy", "sci-fi"},
		Version: 1,
	}

	for b.Loop() {
		rr := httptest.NewRecorder()

		if err := app.writeJSON(rr, http.StatusOK, envelope{"movie": movie}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteJSONList measures a full page of results, which is the largest
// response the API produces.
func BenchmarkWriteJSONList(b *testing.B) {
	app := &application{}

	movies := make([]*data.Movie, 20)
	for i := range movies {
		movies[i] = &data.Movie{
			ID:      int64(i + 1),
			Title:   "A Movie With A Reasonably Long Title",
			Year:    2000 + int32(i),
			Runtime: 120,
			Genres:  data.Genres{"drama", "romance"},
			Version: 1,
		}
	}

	metadata := data.Metadata{
		CurrentPage: 1, PageSize: 20, FirstPage: 1, LastPage: 5, TotalRecords: 100,
	}

	for b.Loop() {
		rr := httptest.NewRecorder()

		err := app.writeJSON(rr, http.StatusOK,
			envelope{"movies": movies, "metadata": metadata}, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReadJSON measures request decoding, including the
// DisallowUnknownFields and second-Decode checks that readJSON layers on top of
// the standard decoder.
func BenchmarkReadJSON(b *testing.B) {
	app := &application{}

	body := `{"title":"Moana","year":2016,"runtime":"107 mins","genres":["animation","adventure"]}`

	for b.Loop() {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/movies", strings.NewReader(body))

		var input struct {
			Title   string       `json:"title"`
			Year    int32        `json:"year"`
			Runtime data.Runtime `json:"runtime"`
			Genres  []string     `json:"genres"`
		}

		if err := app.readJSON(rr, r, &input); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMiddlewareChain measures the cost the middleware stack adds to every
// request, by hitting the healthcheck through the real chain from routes().
//
// Useful as a baseline: if a new middleware lands and this jumps, the cost is
// being paid by every endpoint, not just the one that motivated it.
func BenchmarkMiddlewareChain(b *testing.B) {
	app := &application{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		mailer: &mockMailer{},
	}
	app.config.limiter.enabled = false

	handler := app.routes()

	for b.Loop() {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/healthcheck", nil)

		handler.ServeHTTP(rr, r)
	}
}

// BenchmarkMiddlewareChainWithRateLimit shows what the per-client limiter costs
// — a map lookup behind a mutex on every request, which is the part that would
// become contended under real load.
func BenchmarkMiddlewareChainWithRateLimit(b *testing.B) {
	app := &application{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		mailer: &mockMailer{},
	}
	app.config.limiter.enabled = true
	// High enough that the benchmark never trips it.
	app.config.limiter.rps = 1_000_000
	app.config.limiter.burst = 1_000_000

	handler := app.routes()

	for b.Loop() {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/healthcheck", nil)

		handler.ServeHTTP(rr, r)
	}
}
