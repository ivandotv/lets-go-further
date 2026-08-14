package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"greenlight/internal/assert"
	"greenlight/internal/data"
)

// TestContextSetAndGetUser is the round trip.
func TestContextSetAndGetUser(t *testing.T) {
	app := &application{}

	user := &data.User{ID: 7, Name: "Alice", Email: "alice@example.com", Activated: true}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = app.contextSetUser(r, user)

	got := app.contextGetUser(r)

	// Pointer equality: the middleware must hand the SAME user through, not a
	// copy, so that anything downstream sees later mutations.
	if got != user {
		t.Errorf("got %+v; want the same *data.User that was stored (%+v)", got, user)
	}
}

// TestContextGetUserPanicsWhenMissing covers the deliberate panic.
//
// It reads oddly to assert that code panics, but the panic is the contract:
// contextGetUser is only ever called from handlers that sit behind the
// authenticate middleware, which always sets either a real user or
// data.AnonymousUser. Reaching it without one means the middleware chain in
// routes.go is wired wrong.
//
// Returning nil instead would turn that wiring bug into a nil-pointer
// dereference somewhere further down the call stack, where the cause is much
// harder to see. Panicking here is caught by recoverPanic and logged with a
// stack trace pointing straight at the real problem.
func TestContextGetUserPanicsWhenMissing(t *testing.T) {
	app := &application{}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("contextGetUser did not panic on a request with no user in its context")
		}

		msg, ok := r.(string)
		if !ok {
			t.Fatalf("got a panic value of type %T; want a string", r)
		}

		assert.Equal(t, msg, "missing user value in request context")
	}()

	app.contextGetUser(httptest.NewRequest(http.MethodGet, "/", nil))
}

// TestContextGetUserPanicsOnWrongType covers the other half of the comma-ok
// assertion: a value stored under the key that isn't a *data.User.
//
// This can only happen if something outside this package writes to the same
// key — which is precisely what the custom contextKey type exists to prevent.
// The test documents that even if it somehow did, the failure is loud.
func TestContextGetUserPanicsOnWrongType(t *testing.T) {
	app := &application{}

	defer func() {
		if recover() == nil {
			t.Error("contextGetUser did not panic when the context value was the wrong type")
		}
	}()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := r.Context()

	// Deliberately store a string rather than a *data.User.
	r = r.WithContext(context.WithValue(ctx, userContextKey, "not a user"))

	app.contextGetUser(r)
}

// TestContextSetAndGetRequestID is the round trip.
func TestContextSetAndGetRequestID(t *testing.T) {
	app := &application{}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = app.contextSetRequestID(r, "abc123")

	assert.Equal(t, app.contextGetRequestID(r), "abc123")
}

// TestContextGetRequestIDDefaultsWhenMissing covers the deliberate difference
// from contextGetUser: a request built without going through the requestID
// middleware — which is how errors_test.go exercises the error helpers
// directly — must get a harmless empty string back, not a panic.
func TestContextGetRequestIDDefaultsWhenMissing(t *testing.T) {
	app := &application{}

	got := app.contextGetRequestID(httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, got, "")
}
