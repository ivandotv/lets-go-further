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
