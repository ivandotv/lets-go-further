package main

import (
	"net/http"
	"testing"

	"greenlight/internal/assert"
)

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
		defer func() { _ = rs.Body.Close() }()

		assert.Equal(t, rs.Header.Get("Access-Control-Allow-Origin"), "https://trusted.example.com")
	})

	t.Run("untrusted origin gets no CORS header", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/healthcheck", nil)
		assert.NilError(t, err)
		req.Header.Set("Origin", "https://evil.example.com")

		rs, err := ts.Client().Do(req)
		assert.NilError(t, err)
		defer func() { _ = rs.Body.Close() }()

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
		defer func() { _ = rs.Body.Close() }()

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
