package main

import (
	"fmt"
	"net/http"
)

// Every middleware here has the same shape:
//
//	func (app *application) name(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        // ...do something before...
//	        next.ServeHTTP(w, r)
//	        // ...do something after...
//	    })
//	}
//
// That signature — take a Handler, return a Handler — is what makes them
// composable. See routes.go for how they're chained together.

// recoverPanic converts a panic in a handler into a clean 500 response.
//
// Without it, a panic unwinds the goroutine and Go's http.Server logs the stack
// trace and closes the connection abruptly — the client sees an empty reply
// with no status code at all.
//
// IMPORTANT LIMITATION: this only recovers panics in the goroutine handling
// THIS request. A panic in a goroutine you spawn will still crash the whole
// process, which is exactly why app.background() has its own recover.
func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deferred functions run as the stack unwinds during a panic, which is
		// the only place recover() does anything.
		defer func() {
			if err := recover(); err != nil {
				// Tell Go's server to close this connection once the response
				// is sent. The connection's state is unknown after a panic, so
				// reusing it for keep-alive would be risky.
				w.Header().Set("Connection", "close")

				// recover() returns `any`, so we normalise it into an error.
				// %v handles whatever was panicked with — string, error, or
				// anything else.
				app.serverErrorResponse(w, r, fmt.Errorf("%v", err))
			}
		}()

		next.ServeHTTP(w, r)
	})
}
