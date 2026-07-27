package main

import (
	"expvar"
	"net/http"

	"github.com/julienschmidt/httprouter"
	"github.com/justinas/alice"

	"greenlight/internal/data"
)

// routes builds the application's router and wraps it in the middleware chain.
//
// It returns http.Handler (not *httprouter.Router) because by the time we're
// done, the router is buried inside several layers of middleware.
func (app *application) routes() http.Handler {
	router := httprouter.New()

	// ── Router-level error handling ──────────────────────────────────────────
	//
	// httprouter has its own built-in 404 and 405 responses, which return
	// plain text. We override them so that EVERY response from the API — even
	// for a URL that matches no route — is JSON with our envelope shape. A
	// client should never have to special-case a text/plain error.
	//
	// http.HandlerFunc(...) converts our method into a http.Handler.
	router.NotFound = http.HandlerFunc(app.notFoundResponse)
	router.MethodNotAllowed = http.HandlerFunc(app.methodNotAllowedResponse)

	// ── Routes ───────────────────────────────────────────────────────────────
	//
	// Note how the permissions are expressed right here in the routing table.
	// You can read off the entire authorization model at a glance, which is
	// much harder when the checks are buried inside each handler.

	router.HandlerFunc(http.MethodGet, "/v1/healthcheck", app.healthcheckHandler)

	// Movies. Reading requires the movies:read permission; writing requires
	// movies:write. Both imply an activated, authenticated account.
	router.HandlerFunc(http.MethodGet, "/v1/movies",
		app.requirePermission(data.PermissionMoviesRead, app.listMoviesHandler))
	router.HandlerFunc(http.MethodPost, "/v1/movies",
		app.requirePermission(data.PermissionMoviesWrite, app.createMovieHandler))
	router.HandlerFunc(http.MethodGet, "/v1/movies/:id",
		app.requirePermission(data.PermissionMoviesRead, app.showMovieHandler))
	// PATCH rather than PUT, because updates are partial — see updateMovieHandler.
	router.HandlerFunc(http.MethodPatch, "/v1/movies/:id",
		app.requirePermission(data.PermissionMoviesWrite, app.updateMovieHandler))
	router.HandlerFunc(http.MethodDelete, "/v1/movies/:id",
		app.requirePermission(data.PermissionMoviesWrite, app.deleteMovieHandler))

	// Users. These are necessarily public — you can't require authentication
	// to register.
	router.HandlerFunc(http.MethodPost, "/v1/users", app.registerUserHandler)
	router.HandlerFunc(http.MethodPut, "/v1/users/activated", app.activateUserHandler)

	// Tokens. Exchanging credentials for a bearer token is also public.
	router.HandlerFunc(http.MethodPost, "/v1/tokens/authentication", app.createAuthenticationTokenHandler)

	// Runtime metrics. expvar.Handler() serves the JSON at /debug/vars.
	//
	// SECURITY: this exposes memory statistics, the command line the process
	// was started with, and database pool internals. In production it should be
	// behind authentication or bound to an internal-only interface. It's left
	// open here because this is a learning project.
	router.Handler(http.MethodGet, "/debug/vars", expvar.Handler())

	// ── The middleware chain ─────────────────────────────────────────────────
	//
	// justinas/alice (from the first book) turns nested wrapping like:
	//
	//     app.metrics(app.recoverPanic(app.enableCORS(app.rateLimit(router))))
	//
	// ...into a readable left-to-right list. The order is significant, and
	// it's outermost-first:
	//
	//  1. metrics        — count everything, including rejected requests
	//  2. recoverPanic   — must be near the outside so it catches panics from
	//                      every middleware below it
	//  3. secureHeaders  — cheap, applies to all responses
	//  4. enableCORS     — must run before rateLimit, so that a rejected
	//                      preflight still gets CORS headers (otherwise the
	//                      browser reports a confusing CORS error instead of
	//                      the real 429)
	//  5. rateLimit      — reject floods before doing any real work
	//  6. authenticate   — identify the user for downstream handlers
	//  7. logRequest     — innermost, so the logged status is the final one
	//
	// Then finally the router itself.
	standard := alice.New(
		app.metrics,
		app.recoverPanic,
		app.secureHeaders,
		app.enableCORS,
		app.rateLimit,
		app.authenticate,
		app.logRequest,
	)

	return standard.Then(router)
}
