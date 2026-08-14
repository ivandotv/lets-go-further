package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// requestID assigns a random correlation ID to every request, returns it in
// the X-Request-Id response header, and stores it in the request's context so
// every log line produced while handling the request can carry it too.
//
// It runs OUTERMOST in the chain (see routes.go) — deliberately before
// recoverPanic — so that even the log line recoverPanic writes when it
// recovers a handler panic has the same ID as everything else that request
// produced.
//
// Note that any X-Request-Id the CLIENT sent is ignored rather than adopted.
// Trusting it would let a caller collapse unrelated requests onto one ID (or
// forge another caller's), which makes the logs actively misleading. Behind a
// trusted proxy that already assigns IDs you'd want the opposite — but that
// only holds when something upstream is guaranteed to overwrite the header,
// which is the same assumption rateLimit's X-Forwarded-For handling warns
// about.
func (app *application) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := generateRequestID()

		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, app.contextSetRequestID(r, id))
	})
}

// generateRequestID returns 16 random bytes, hex-encoded to 32 characters.
//
// Unlike data.generateToken this returns no error, because there is no failure
// to report: since Go 1.24 crypto/rand.Read either fills the slice completely
// or crashes the program irrecoverably, so an error path here would be dead
// code.
//
// It's also worth being clear that this is a correlation aid, not a
// credential — nothing is authorised by it, and it's echoed to the client on
// purpose. 128 bits is far more than collision-avoidance needs; it's used
// because it's the obvious size and costs nothing.
func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}
