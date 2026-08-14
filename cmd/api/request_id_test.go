package main

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"greenlight/internal/assert"
)

// TestRequestIDIsReturnedAndUnique checks the two properties that make the ID
// useful: a client can read it off the response (so it can quote the ID when
// reporting a problem), and two requests never share one.
func TestRequestIDIsReturnedAndUnique(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	seen := make(map[string]bool)

	for range 5 {
		_, headers, _ := ts.get(t, "/v1/healthcheck", "")

		id := headers.Get("X-Request-Id")

		// 16 random bytes, hex-encoded.
		raw, err := hex.DecodeString(id)
		assert.NilError(t, err)
		assert.Equal(t, len(raw), 16)

		if seen[id] {
			t.Fatalf("request ID %q was issued twice — IDs must be unique per request", id)
		}
		seen[id] = true
	}
}

// TestRequestIDIsSetOnErrorResponsesToo checks the case that actually matters.
//
// A client only needs the ID when something went wrong, so an implementation
// that set the header on the success path but lost it on a 404 or a 405 would
// be useless in exactly the situation it exists for. requestID sits outermost
// in the chain precisely so this holds no matter which layer produced the
// response.
func TestRequestIDIsSetOnErrorResponsesToo(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	t.Run("404", func(t *testing.T) {
		_, headers, _ := ts.get(t, "/v1/nonexistent", "")
		assert.Equal(t, len(headers.Get("X-Request-Id")), 32)
	})

	t.Run("405", func(t *testing.T) {
		_, headers, _ := ts.delete(t, "/v1/healthcheck", "")
		assert.Equal(t, len(headers.Get("X-Request-Id")), 32)
	})

	t.Run("401", func(t *testing.T) {
		_, headers, _ := ts.get(t, "/v1/movies", "")
		assert.Equal(t, len(headers.Get("X-Request-Id")), 32)
	})
}

// TestRequestIDIsSetOnRateLimitedResponses guards the ORDERING in routes.go,
// which the tests above don't.
//
// A 429 is produced by rateLimit itself: the request is rejected without ever
// reaching the router, so the header can only be present if requestID sits
// outside the limiter in the real chain. Move requestID below rateLimit and
// this goes red while everything else stays green — and a client being
// throttled would lose exactly the ID it needs to quote when asking why.
func TestRequestIDIsSetOnRateLimitedResponses(t *testing.T) {
	app, _ := newTestApplication(t)

	app.config.limiter.enabled = true
	app.config.limiter.rps = 1
	app.config.limiter.burst = 1

	ts := newTestServer(t, app.routes())

	// Burn the single token, then trip the limiter.
	ts.get(t, "/v1/healthcheck", "")

	code, headers, _ := ts.get(t, "/v1/healthcheck", "")

	assert.Equal(t, code, http.StatusTooManyRequests)
	assert.Equal(t, len(headers.Get("X-Request-Id")), 32)
}

// TestRequestIDCorrelatesResponseWithLog is the point of the whole feature.
//
// The ID a client is handed in the header must be the SAME one that appears in
// the server's log line for that request — otherwise it correlates nothing.
// This is easy to break by, say, generating the ID twice, and nothing else in
// the suite would notice.
func TestRequestIDCorrelatesResponseWithLog(t *testing.T) {
	var logBuf safeBuffer

	app, _ := newTestApplication(t)
	app.logger = newCapturingLogger(&logBuf)

	ts := newTestServer(t, app.routes())

	_, headers, _ := ts.get(t, "/v1/healthcheck", "")

	id := headers.Get("X-Request-Id")
	if id == "" {
		t.Fatal("no X-Request-Id header on the response")
	}

	// logRequest writes its line after the handler returns, which can be after
	// the client has the response. Close blocks until the request is fully
	// done, so the line is guaranteed to be there.
	ts.Close()

	// logRequest emits one JSON object per completed request.
	var entry struct {
		Msg       string `json:"msg"`
		RequestID string `json:"request_id"`
		URI       string `json:"uri"`
	}

	logged := logBuf.String()

	if err := json.Unmarshal([]byte(strings.TrimSpace(logged)), &entry); err != nil {
		t.Fatalf("could not parse the request log line as JSON: %s", logged)
	}

	assert.Equal(t, entry.Msg, "request")
	assert.Equal(t, entry.URI, "/v1/healthcheck")
	assert.Equal(t, entry.RequestID, id)
}

// TestRequestIDReachesPanicLog checks the ordering decision in routes.go.
//
// requestID is placed OUTSIDE recoverPanic so that the 500 a client receives
// after a handler panics carries an ID that also appears in the logged error.
// A panic is the case where correlating a report with a stack trace matters
// most, and it's the one an inside-out ordering would silently lose.
func TestRequestIDReachesPanicLog(t *testing.T) {
	var logBuf safeBuffer

	app, _ := newTestApplication(t)
	app.logger = newCapturingLogger(&logBuf)

	// recoverPanic is what turns the panic into a 500; requestID wraps it.
	handler := app.requestID(app.recoverPanic(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			panic("something went badly wrong")
		},
	)))

	ts := newTestServer(t, handler)

	code, headers, _ := ts.get(t, "/", "")

	assert.Equal(t, code, http.StatusInternalServerError)

	id := headers.Get("X-Request-Id")
	if id == "" {
		t.Fatal("no X-Request-Id header on the 500 response")
	}

	ts.Close()

	logged := logBuf.String()

	assert.StringContains(t, logged, id)
	assert.StringContains(t, logged, "something went badly wrong")
}
