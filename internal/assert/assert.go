// Package assert provides tiny test-assertion helpers.
//
// This comes from the FIRST book ("Let's Go", chapter 13). Go deliberately has
// no assertion library in its standard library — the official position is that
// `if got != want { t.Errorf(...) }` is clear enough. That's true, but it gets
// repetitive, and these three generic helpers remove the repetition without
// pulling in a dependency like testify.
//
// Every helper calls t.Helper(), which is the important bit: it tells the
// testing package to report failures against the CALLER's line number rather
// than the line inside this file. Without it, every failure would point here
// and be useless.
package assert

import (
	"strings"
	"testing"
)

// Equal fails the test if got != want.
//
// The type parameter is constrained to `comparable`, which is the set of types
// that support == . That covers strings, numbers, bools, pointers and structs
// of comparable fields — but NOT slices or maps, which is why there's a
// separate helper below.
func Equal[T comparable](t *testing.T, got, want T) {
	t.Helper()

	if got != want {
		t.Errorf("got: %v; want: %v", got, want)
	}
}

// StringContains fails the test if got doesn't contain the substring want.
//
// Useful for checking that a response body mentions a particular error message
// without pinning the test to the exact wording of the whole body.
func StringContains(t *testing.T, got, want string) {
	t.Helper()

	if !strings.Contains(got, want) {
		t.Errorf("got: %q; expected to contain: %q", got, want)
	}
}

// NilError fails the test — and STOPS it — if err is not nil.
//
// Note this uses t.Fatal, not t.Error. The distinction matters: Fatal aborts
// the test immediately. If a setup step returned an error, continuing would
// just produce a cascade of confusing follow-on failures (usually a nil
// pointer dereference).
func NilError(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("got unexpected error: %v", err)
	}
}
