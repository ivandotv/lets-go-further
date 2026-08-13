package main

import (
	"net/http"
	"testing"

	"greenlight/internal/assert"
)

// TestHealthcheck is the simplest possible end-to-end test, and a good one to
// read first: it builds the real application, starts a real server, makes a real
// HTTP request, and asserts on the real response.
//
// It's also the smoke test for the whole harness. If newTestApplication or
// newTestServer (both in testutils_test.go) break, this fails alongside
// everything else — but it's the one failure that's obviously about the plumbing
// rather than about the endpoint.
//
// The empty string is the auth token: the healthcheck is deliberately public, so
// it's called with no credentials. See TestHealthcheckReportsVersionAndEnv in
// main_test.go for the rest of the payload.
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
