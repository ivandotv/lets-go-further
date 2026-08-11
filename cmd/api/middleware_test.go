package main

import (
	"io"
	"net/http"
	"testing"

	"greenlight/internal/assert"
)

func TestHealthcheck(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	code, headers, body := ts.get(t, "/v1/healthcheck", "")

	assert.Equal(t, code, http.StatusOK)
	assert.Equal(t, headers.Get("Content-Type"), "application/json")
	assert.StringContains(t, body, `"status": "available"`)
	assert.StringContains(t, body, `"environment": "testing"`)
}

// TestNotFoundAndMethodNotAllowed checks that http.ServeMux's default
// plain-text 404/405 responses were successfully overridden with our JSON
// ones — see the fallback patterns registered in routes.go.
//
// A client should never have to handle two different error formats.
func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	t.Run("unknown route returns JSON 404", func(t *testing.T) {
		code, headers, body := ts.get(t, "/v1/nonexistent", "")

		assert.Equal(t, code, http.StatusNotFound)
		assert.Equal(t, headers.Get("Content-Type"), "application/json")
		assert.StringContains(t, body, "could not be found")
	})

	t.Run("wrong method returns JSON 405", func(t *testing.T) {
		// /v1/healthcheck exists, but only for GET.
		code, headers, body := ts.delete(t, "/v1/healthcheck", "")

		assert.Equal(t, code, http.StatusMethodNotAllowed)
		assert.Equal(t, headers.Get("Content-Type"), "application/json")
		assert.StringContains(t, body, "not supported for this resource")
	})
}

func TestSecureHeaders(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	_, headers, _ := ts.get(t, "/v1/healthcheck", "")

	tests := []struct {
		header string
		want   string
	}{
		{header: "X-Content-Type-Options", want: "nosniff"},
		{header: "X-Frame-Options", want: "deny"},
		{header: "Referrer-Policy", want: "origin-when-cross-origin"},
		{header: "Content-Security-Policy", want: "default-src 'none'; frame-ancestors 'none'"},
	}

	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			assert.Equal(t, headers.Get(tt.header), tt.want)
		})
	}
}

// TestCORS checks the origin safelist. The negative case matters most: an
// untrusted origin must NOT receive an Access-Control-Allow-Origin header,
// because that header is the entire access control mechanism.
func TestCORS(t *testing.T) {
	app, _ := newTestApplication(t)
	// newTestApplication trusts exactly https://trusted.example.com
	ts := newTestServer(t, app.routes())

	t.Run("trusted origin gets the CORS header", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/healthcheck", nil)
		assert.NilError(t, err)
		req.Header.Set("Origin", "https://trusted.example.com")

		rs, err := ts.Client().Do(req)
		assert.NilError(t, err)
		defer rs.Body.Close()

		assert.Equal(t, rs.Header.Get("Access-Control-Allow-Origin"), "https://trusted.example.com")
	})

	t.Run("untrusted origin gets no CORS header", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/healthcheck", nil)
		assert.NilError(t, err)
		req.Header.Set("Origin", "https://evil.example.com")

		rs, err := ts.Client().Do(req)
		assert.NilError(t, err)
		defer rs.Body.Close()

		// The request still succeeds — CORS is enforced by the BROWSER, not the
		// server. What matters is that we don't hand out permission.
		assert.Equal(t, rs.StatusCode, http.StatusOK)
		assert.Equal(t, rs.Header.Get("Access-Control-Allow-Origin"), "")
	})

	t.Run("preflight from a trusted origin is answered", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodOptions, ts.URL+"/v1/movies", nil)
		assert.NilError(t, err)
		req.Header.Set("Origin", "https://trusted.example.com")
		// This header is what makes an OPTIONS request a *preflight*.
		req.Header.Set("Access-Control-Request-Method", "POST")

		rs, err := ts.Client().Do(req)
		assert.NilError(t, err)
		defer rs.Body.Close()

		assert.Equal(t, rs.StatusCode, http.StatusOK)
		assert.Equal(t, rs.Header.Get("Access-Control-Allow-Origin"), "https://trusted.example.com")
		assert.StringContains(t, rs.Header.Get("Access-Control-Allow-Methods"), "PATCH")
		assert.StringContains(t, rs.Header.Get("Access-Control-Allow-Headers"), "Authorization")
	})

	t.Run("Vary headers are set for cache correctness", func(t *testing.T) {
		_, headers, _ := ts.get(t, "/v1/healthcheck", "")

		// Without Vary: Origin, a shared cache could serve the trusted
		// origin's response (complete with its CORS header) to an untrusted
		// one — silently defeating the safelist.
		vary := headers.Values("Vary")

		var hasOrigin, hasAuthorization bool
		for _, v := range vary {
			switch v {
			case "Origin":
				hasOrigin = true
			case "Authorization":
				hasAuthorization = true
			}
		}

		assert.Equal(t, hasOrigin, true)
		assert.Equal(t, hasAuthorization, true)
	})
}

// TestRateLimit turns the limiter on (it's off for the other tests) and
// confirms it actually rejects a flood.
func TestRateLimit(t *testing.T) {
	app, _ := newTestApplication(t)

	// 2 requests per second with a burst of 3: the first 3 go through
	// immediately, and the 4th should be rejected because the bucket is empty
	// and less than half a second has passed.
	app.config.limiter.enabled = true
	app.config.limiter.rps = 2
	app.config.limiter.burst = 3

	ts := newTestServer(t, app.routes())

	// Drain the burst.
	for i := range 3 {
		code, _, _ := ts.get(t, "/v1/healthcheck", "")
		if code != http.StatusOK {
			t.Fatalf("request %d: got %d; want 200 (still within the burst)", i+1, code)
		}
	}

	// The next one must be rejected.
	code, _, body := ts.get(t, "/v1/healthcheck", "")
	assert.Equal(t, code, http.StatusTooManyRequests)
	assert.StringContains(t, body, "rate limit exceeded")
}

// TestRateLimitDisabled is the control for the test above — it proves the
// `enabled` flag is actually consulted, rather than the limiter simply never
// firing.
func TestRateLimitDisabled(t *testing.T) {
	app, _ := newTestApplication(t)
	app.config.limiter.enabled = false

	ts := newTestServer(t, app.routes())

	for i := range 20 {
		code, _, _ := ts.get(t, "/v1/healthcheck", "")
		if code != http.StatusOK {
			t.Fatalf("request %d: got %d; want 200 with the limiter disabled", i+1, code)
		}
	}
}

// TestRecoverPanic verifies that a panicking handler produces a clean JSON 500
// instead of an abruptly-closed connection.
func TestRecoverPanic(t *testing.T) {
	app, _ := newTestApplication(t)

	// Build a minimal chain around a handler that panics, rather than adding a
	// panicking route to the real router.
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something went badly wrong")
	})

	ts := newTestServer(t, app.recoverPanic(panicky))

	// We make the request directly rather than via ts.get, because we need the
	// *http.Response itself — see the Close assertion below.
	rs, err := ts.Client().Get(ts.URL + "/")
	assert.NilError(t, err)
	defer rs.Body.Close()

	body, err := io.ReadAll(rs.Body)
	assert.NilError(t, err)

	assert.Equal(t, rs.StatusCode, http.StatusInternalServerError)

	// The middleware sets `Connection: close`, because the connection's state
	// is unknown after a panic and reusing it for keep-alive would be risky.
	//
	// Note we assert on rs.Close, NOT on the Connection header: Connection is
	// a hop-by-hop header, so Go's HTTP transport consumes it and reports it
	// as this boolean instead of leaving it in rs.Header. Checking the header
	// would silently always see "" and prove nothing.
	assert.Equal(t, rs.Close, true)

	// Critically: the client gets our generic message, NOT the panic value.
	// Leaking internal detail — stack traces, SQL, file paths — is an
	// information disclosure vulnerability.
	assert.StringContains(t, string(body), "the server encountered a problem")
	assert.Equal(t, containsAny(string(body), "something went badly wrong"), false)
}

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
