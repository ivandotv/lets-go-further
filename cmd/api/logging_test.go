package main

import (
	"net/http"
	"testing"

	"greenlight/internal/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// OBSERVABILITY MIDDLEWARE
// ─────────────────────────────────────────────────────────────────────────────
//
// logging.go holds two middlewares, and only one of them is asserted on here.
//
// logRequest is EXECUTED by every test in this package — it's in the chain
// routes() builds — but nothing checks what it logged, because
// newTestApplication points the logger at io.Discard to keep a passing run
// readable. That's a deliberate trade rather than an oversight: the log lines
// aren't a contract anyone programs against, so pinning their format would
// mostly create work when someone adds a field. (The one place log OUTPUT is
// asserted is errors_test.go, where the point is that internal error detail
// reaches the log instead of the client.)
//
// The metrics middleware is different: /debug/vars is a real endpoint with real
// consumers, so it gets a real test.

// TestMetricsEndpoint checks that expvar is wired up and counting.
//
// Worth knowing why the counters are package-level vars in logging.go rather
// than declared inside the middleware as the book has them: expvar.NewInt
// panics on a duplicate name, and this suite builds a fresh application — and
// therefore a fresh chain — for every test. Declared per-chain, the SECOND test
// in this package would panic. So this test passing at all depends on that
// detail, which is the sort of thing worth noticing when it looks like a
// pointless deviation from the book.
func TestMetricsEndpoint(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	// Generate some traffic to count.
	ts.get(t, "/v1/healthcheck", "")
	ts.get(t, "/v1/nonexistent", "")

	code, _, body := ts.get(t, "/debug/vars", "")

	assert.Equal(t, code, http.StatusOK)

	// Only the counters registered by the metrics middleware are asserted
	// here. The "version", "goroutines", "database" and "timestamp" vars are
	// published in run() rather than by the middleware, so they don't exist in
	// a test-constructed application.
	for _, key := range []string{
		"total_requests_received",
		"total_responses_sent",
		"total_processing_time_μs",
		"total_responses_sent_by_status",
	} {
		assert.StringContains(t, body, key)
	}

	// The status-code map should reflect the traffic we just generated: a 200
	// from the healthcheck and a 404 from the unknown route.
	assert.StringContains(t, body, `"200"`)
	assert.StringContains(t, body, `"404"`)
}
