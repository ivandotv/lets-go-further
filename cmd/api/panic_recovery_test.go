package main

import (
	"io"
	"net/http"
	"testing"

	"greenlight/internal/assert"
)

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
