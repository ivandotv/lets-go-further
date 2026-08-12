package main

import (
	"errors"
	"net/http"
	"strings"

	"greenlight/internal/data"
	"greenlight/internal/validator"
)

// authenticate identifies the user making the request, if any.
//
// It runs on EVERY request and always sets a user in the context — either a
// real one or data.AnonymousUser. Downstream middleware and handlers can then
// rely on contextGetUser never failing.
//
// Note that this middleware doesn't reject anyone; it only identifies. Enforcing
// access is the job of requireAuthenticatedUser and friends below. Splitting
// "who are you" from "are you allowed" keeps each piece simple.
func (app *application) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tell caches that the response varies by Authorization header, so a
		// proxy can't serve one user's response to another.
		w.Header().Add("Vary", "Authorization")

		authorizationHeader := r.Header.Get("Authorization")

		// No header at all → anonymous. This is not an error: plenty of
		// endpoints are public.
		if authorizationHeader == "" {
			r = app.contextSetUser(r, data.AnonymousUser)
			next.ServeHTTP(w, r)
			return
		}

		// Expected format is exactly: "Bearer <token>"
		headerParts := strings.Split(authorizationHeader, " ")
		if len(headerParts) != 2 || headerParts[0] != "Bearer" {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		token := headerParts[1]

		// Cheap shape check before touching the database.
		v := validator.New()
		if data.ValidateTokenPlaintext(v, token); !v.Valid() {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		user, err := app.models.Users.GetForToken(data.ScopeAuthentication, token)
		if err != nil {
			switch {
			case errors.Is(err, data.ErrRecordNotFound):
				app.invalidAuthenticationTokenResponse(w, r)
			default:
				app.serverErrorResponse(w, r, err)
			}
			return
		}

		r = app.contextSetUser(r, user)

		next.ServeHTTP(w, r)
	})
}

// requireAuthenticatedUser rejects anonymous requests with a 401.
//
// Note the signature: it takes and returns http.HandlerFunc rather than
// http.Handler. That's so these can be composed directly around a handler
// function in routes.go without extra conversions.
func (app *application) requireAuthenticatedUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		if user.IsAnonymous() {
			app.authenticationRequiredResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}
}

// requireActivatedUser rejects users who haven't activated their account.
//
// It WRAPS requireAuthenticatedUser rather than repeating the anonymous check,
// so "activated" automatically implies "authenticated". Composing the checks
// like this makes it impossible to accidentally allow an anonymous user
// through by forgetting a wrapper in routes.go.
func (app *application) requireActivatedUser(next http.HandlerFunc) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		if !user.Activated {
			app.inactiveAccountResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}

	return app.requireAuthenticatedUser(fn)
}

// requirePermission checks that the user holds a specific permission code.
//
// Like the above, it composes: permission implies activated implies
// authenticated.
func (app *application) requirePermission(code string, next http.HandlerFunc) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		permissions, err := app.models.Permissions.GetAllForUser(user.ID)
		if err != nil {
			app.serverErrorResponse(w, r, err)
			return
		}

		if !permissions.Include(code) {
			app.notPermittedResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
		//note: can also be
		// next(w, r)
	}

	return app.requireActivatedUser(fn)
}
