package main

import (
	"expvar"
	"net/http"

	"github.com/justinas/alice"

	"greenlight/internal/data"
)

// routes builds the application's router and wraps it in the middleware chain.
//
// It returns http.Handler (not *http.ServeMux) because by the time we're done,
// the router is buried inside several layers of middleware.
func (app *application) routes() http.Handler {
	mux := http.NewServeMux()

	// ── Router-level error handling ──────────────────────────────────────────
	//
	// http.ServeMux has no NotFound/MethodNotAllowed hooks like httprouter did,
	// so we get JSON versions of both by registering fallback patterns instead:
	//
	//  - A method-less pattern (e.g. "/v1/movies") is a strict superset of the
	//    method-specific ones registered for the same path (e.g. "GET
	//    /v1/movies"). Per ServeMux's precedence rules the more specific,
	//    method-specific pattern wins whenever it matches, so the fallback only
	//    ever fires for a method nobody registered — exactly the 405 case.
	//  - "/" is a catch-all matching any path with no more specific pattern
	//    registered, which covers the 404 case.
	//
	// A client should never have to special-case a text/plain error.
	mux.HandleFunc("/", app.notFoundResponse)

	// ── Routes ───────────────────────────────────────────────────────────────
	//
	// Note how the permissions are expressed right here in the routing table.
	// You can read off the entire authorization model at a glance, which is
	// much harder when the checks are buried inside each handler.

	mux.HandleFunc("GET /v1/healthcheck", app.healthcheckHandler)
	mux.HandleFunc("/v1/healthcheck", app.methodNotAllowedResponse)

	// Movies. Reading requires the movies:read permission; writing requires
	// movies:write. Both imply an activated, authenticated account.
	mux.HandleFunc("GET /v1/movies",
		app.requirePermission(data.PermissionMoviesRead, app.listMoviesHandler))
	mux.HandleFunc("POST /v1/movies",
		app.requirePermission(data.PermissionMoviesWrite, app.createMovieHandler))
	mux.HandleFunc("/v1/movies", app.methodNotAllowedResponse)

	mux.HandleFunc("GET /v1/movies/{id}",
		app.requirePermission(data.PermissionMoviesRead, app.showMovieHandler))
	// PATCH rather than PUT, because updates are partial — see updateMovieHandler.
	mux.HandleFunc("PATCH /v1/movies/{id}",
		app.requirePermission(data.PermissionMoviesWrite, app.updateMovieHandler))
	mux.HandleFunc("DELETE /v1/movies/{id}",
		app.requirePermission(data.PermissionMoviesWrite, app.deleteMovieHandler))
	mux.HandleFunc("/v1/movies/{id}", app.methodNotAllowedResponse)

	// Users. These are necessarily public — you can't require authentication
	// to register.
	mux.HandleFunc("POST /v1/users", app.registerUserHandler)
	mux.HandleFunc("/v1/users", app.methodNotAllowedResponse)

	mux.HandleFunc("PUT /v1/users/activated", app.activateUserHandler)
	mux.HandleFunc("/v1/users/activated", app.methodNotAllowedResponse)

	// Tokens. Exchanging credentials for a bearer token is also public.
	mux.HandleFunc("POST /v1/tokens/authentication", app.createAuthenticationTokenHandler)
	mux.HandleFunc("/v1/tokens/authentication", app.methodNotAllowedResponse)

	// Runtime metrics. expvar.Handler() serves the JSON at /debug/vars.
	//
	// SECURITY: this exposes memory statistics, the command line the process
	// was started with, and database pool internals. In production it should be
	// behind authentication or bound to an internal-only interface. It's left
	// open here because this is a learning project.
	mux.Handle("GET /debug/vars", expvar.Handler())
	mux.HandleFunc("/debug/vars", app.methodNotAllowedResponse)

	// ── The middleware chain ─────────────────────────────────────────────────
	//
	// justinas/alice (from the first book) turns nested wrapping like:
	//
	//     app.metrics(app.recoverPanic(app.enableCORS(app.rateLimit(mux))))
	//
	// ...into a readable left-to-right list. The order is significant, and
	// it's outermost-first:
	//
	//  1. requestID      — assigns the correlation ID first, so every other
	//                      middleware's log lines (including recoverPanic's)
	//                      can include it
	//  2. metrics        — count everything, including rejected requests
	//  3. recoverPanic   — must be near the outside so it catches panics from
	//                      every middleware below it
	//  4. secureHeaders  — cheap, applies to all responses
	//  5. enableCORS     — must run before rateLimit, so that a rejected
	//                      preflight still gets CORS headers (otherwise the
	//                      browser reports a confusing CORS error instead of
	//                      the real 429)
	//  6. rateLimit      — reject floods before doing any real work
	//  7. authenticate   — identify the user for downstream handlers
	//  8. logRequest     — innermost, so the logged status is the final one
	//
	// Then finally the router itself.
	standard := alice.New(
		app.requestID,
		app.metrics,
		app.recoverPanic,
		app.secureHeaders,
		app.enableCORS,
		app.rateLimit,
		app.authenticate,
		app.logRequest,
	)

	return standard.Then(mux)
}
