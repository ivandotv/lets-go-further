// Package assert provides tiny test-assertion helpers.
//
// This comes from the FIRST book ("Let's Go", chapter 13). Go deliberately has
// no assertion library in its standard library — the official position is that
// `if got != want { t.Errorf(...) }` is clear enough. That's true, but it gets
// repetitive, and these generic helpers remove the repetition without pulling
// in a dependency like testify.
//
// Every helper calls t.Helper(), which is the important bit: it tells the
// testing package to report failures against the CALLER's line number rather
// than the line inside this file. Without it, every failure would point here
// and be useless.
package assert

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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

// DeepEqual fails the test if got and want differ, printing a readable diff.
//
// Equal above is constrained to `comparable`, which rules out exactly the types
// this codebase compares most often: data.Genres (a slice), []*data.Movie, and
// the validator's map[string]string of field errors. Before this helper existed
// tests had to compare those field by field, which is noisy and reports "got 3;
// want 4" rather than showing you WHICH element differs.
//
// go-cmp is the one test dependency this project takes. It's what the Go team
// itself uses, and it panics if called from non-test code, so it can't leak
// into the binary. The argument order is (want, got) inside cmp.Diff but
// (got, want) in this signature, matching Equal above — the "-want +got"
// legend in the output tells you which side is which.
//
// opts forwards go-cmp options, most usefully cmpopts.IgnoreFields for the
// timestamps and versions the database fills in:
//
//	assert.DeepEqual(t, got, want, cmpopts.IgnoreFields(data.Movie{}, "CreatedAt"))
func DeepEqual[T any](t *testing.T, got, want T, opts ...cmp.Option) {
	t.Helper()

	if diff := cmp.Diff(want, got, opts...); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
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

// ErrorIs fails the test if err doesn't match target in the errors.Is sense.
//
// This is the mirror of NilError, for the many tests that assert the model
// layer returned a specific sentinel — ErrRecordNotFound, ErrEditConflict,
// ErrDuplicateEmail, ErrInvalidRuntimeFormat. Those assertions are the point of
// the test, not setup for it, so this reports with t.Error and lets the test
// carry on, the same as Equal above. NilError is the odd one out in using
// Fatal, and for a reason documented there.
//
// errors.Is rather than == because it walks the chain of wrapped errors, so the
// assertion keeps working the moment anyone writes fmt.Errorf("...: %w", Err).
// A bare == would break silently at that point. (%w is what does the wrapping —
// %v flattens the error to a string and defeats Is entirely.)
//
// There's no message parameter, deliberately: t.Helper() means the failure is
// reported against the CALLER's line, which is what distinguishes two identical
// assertions in the same test. Where a failure needs context the line number
// can't supply — which iteration of a loop, which fuzz input — write the
// errors.Is check out by hand and say so in the message.
func ErrorIs(t *testing.T, err, target error) {
	t.Helper()

	if !errors.Is(err, target) {
		t.Errorf("got error: %v; want: %v", err, target)
	}
}
