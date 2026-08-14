package main

import (
	"context"
	"net/http"

	"greenlight/internal/data"
)

// contextKey is a custom type used as the key for values we store in a
// request's context.
//
// ── WHY A CUSTOM TYPE AND NOT JUST A STRING? ─────────────────────────────────
//
// Contexts are shared across every package that touches the request —
// including third-party middleware. If two packages both used the string key
// "user", one would silently overwrite the other, and you'd get a bizarre
// type-assertion failure at runtime with no clue where it came from.
//
// Because contextKey is a *distinct type* defined in this package, a
// contextKey("user") can never collide with a string "user" or with some other
// package's own key type — even one that's also called contextKey. The Go
// vet tool flags basic types used as context keys for exactly this reason.
type contextKey string

// userContextKey is the single key under which we store the authenticated
// User. Declaring it as a constant means there's no chance of a typo producing
// a key that silently doesn't match.
const userContextKey = contextKey("user")

// requestIDContextKey is the key under which the per-request correlation ID
// (see requestID in request_id.go) is stored.
const requestIDContextKey = contextKey("requestID")

// contextSetUser returns a copy of the request with the User added to its
// context.
//
// Note that contexts are IMMUTABLE — you never modify one, you derive a new one
// with WithValue and then swap in a new request that carries it. That's why
// this returns a *http.Request rather than mutating in place.
func (app *application) contextSetUser(r *http.Request, user *data.User) *http.Request {
	ctx := context.WithValue(r.Context(), userContextKey, user)
	return r.WithContext(ctx)
}

// contextGetUser retrieves the User from the request context.
//
// It PANICS if there isn't one. That's deliberate: the authenticate middleware
// runs on every single request and always sets either a real user or
// data.AnonymousUser. So a missing value can only mean the middleware chain is
// misconfigured — a bug we want to find immediately in development rather than
// paper over with a nil return that causes a confusing crash three frames
// later.
func (app *application) contextGetUser(r *http.Request) *data.User {
	// The comma-ok form of a type assertion: `ok` is false rather than
	// panicking if the value is missing or the wrong type.
	user, ok := r.Context().Value(userContextKey).(*data.User)
	if !ok {
		panic("missing user value in request context")
	}

	return user
}

// contextSetRequestID returns a copy of the request with the correlation ID
// added to its context.
func (app *application) contextSetRequestID(r *http.Request, id string) *http.Request {
	ctx := context.WithValue(r.Context(), requestIDContextKey, id)
	return r.WithContext(ctx)
}

// contextGetRequestID retrieves the correlation ID from the request context.
//
// Unlike contextGetUser, this does NOT panic when the value is missing. The
// requestID middleware sets it on every real request, but logError and
// logRequest are also exercised directly in tests against handlers built
// without the full chain (see errors_test.go) — an empty string there just
// means "nothing to correlate", which is a harmless default, not a sign the
// chain is misconfigured.
func (app *application) contextGetRequestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDContextKey).(string)
	return id
}
