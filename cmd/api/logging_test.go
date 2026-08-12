package main

import (
	"net/http"
	"testing"

	"greenlight/internal/assert"
)

// TestMetricsEndpoint checks that expvar is wired up and counting.
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
