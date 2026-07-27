// Package validator provides a tiny, dependency-free helper for collecting
// validation errors.
//
// The idea (straight out of "Let's Go Further", chapter 4) is that instead of
// bailing out on the *first* problem we find, we accumulate every problem into
// a map and send them all back to the client in one response. That's much nicer
// for API consumers: they get to fix everything in one round trip.
//
// The map is keyed by field name, so a response ends up looking like:
//
//	{
//	  "error": {
//	    "title": "must be provided",
//	    "year":  "must be greater than 1888"
//	  }
//	}
package validator

import (
	"regexp"
	"slices"
	"strings"
)

// EmailRX is a (deliberately permissive) regular expression for sanity-checking
// email addresses. It comes from https://html.spec.whatwg.org/#valid-e-mail-address
// — the same pattern browsers use for <input type="email">.
//
// We compile it once at package initialisation rather than on every call,
// because regexp.MustCompile is comparatively expensive.
//
// Note the philosophy here: this is NOT trying to prove an address is
// deliverable (impossible without sending mail). It only catches obvious typos.
var EmailRX = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+\\/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")

// Validator is a container for validation errors.
//
// The zero value is NOT ready to use, because Errors would be a nil map and
// writing to a nil map panics. Always construct one with New().
type Validator struct {
	// Errors maps a field name ("title", "email", ...) to a human-readable
	// message ("must be provided").
	Errors map[string]string
}

// New returns a Validator with an initialised (empty) Errors map.
func New() *Validator {
	return &Validator{Errors: make(map[string]string)}
}

// Valid reports whether the Errors map is empty. If it returns true, everything
// checked so far passed.
func (v *Validator) Valid() bool {
	return len(v.Errors) == 0
}

// AddError records an error message for a given key.
//
// Note the "if _, exists" guard: we keep the FIRST message recorded for a field
// rather than letting later checks overwrite it. This matters because checks
// usually run cheapest-first ("must be provided" before "must be at least 5
// bytes long"), and the first failure is normally the most useful message.
func (v *Validator) AddError(key, message string) {
	if _, exists := v.Errors[key]; !exists {
		v.Errors[key] = message
	}
}

// Check adds an error message to the map *only if* ok is false.
//
// This is the workhorse of the package, and it's what lets validation code read
// almost like a specification:
//
//	v.Check(movie.Title != "", "title", "must be provided")
//	v.Check(len(movie.Title) <= 500, "title", "must not be more than 500 bytes long")
//
// Because the condition is evaluated eagerly by the caller, every Check in a
// block runs — which is exactly what we want, since we're collecting all the
// errors rather than short-circuiting on the first.
func (v *Validator) Check(ok bool, key, message string) {
	if !ok {
		v.AddError(key, message)
	}
}

// PermittedValue reports whether value equals one of the permittedValues.
//
// This is a generic function, so it works for any comparable type — strings,
// ints, whatever. We use it for things like validating the "sort" query
// parameter against a safelist of allowed columns.
//
// (In the book this was written by hand with a for loop; the standard library's
// slices.Contains does the same job in one line since Go 1.21.)
func PermittedValue[T comparable](value T, permittedValues ...T) bool {
	return slices.Contains(permittedValues, value)
}

// Matches reports whether a string matches a regular expression.
func Matches(value string, rx *regexp.Regexp) bool {
	return rx.MatchString(value)
}

// Unique reports whether all values in a slice are distinct.
//
// The implementation uses a set (a map with empty-struct values, which occupy
// zero bytes) so it runs in O(n) rather than the O(n²) you'd get from a nested
// loop.
func Unique[T comparable](values []T) bool {
	uniqueValues := make(map[T]struct{}, len(values))

	for _, value := range values {
		uniqueValues[value] = struct{}{}
	}

	return len(values) == len(uniqueValues)
}

// NotBlank reports whether a string contains a non-whitespace character.
//
// This one is borrowed from the first book ("Let's Go"), where it's used for
// HTML form validation. It's handy any time " " shouldn't count as "provided".
func NotBlank(value string) bool {
	return strings.TrimSpace(value) != ""
}

// MaxBytes reports whether a string is at most n bytes long.
//
// Careful: this counts BYTES, not characters. "héllo" is 5 characters but 6
// bytes in UTF-8. We use bytes here because that's what the database column
// limits are expressed in.
func MaxBytes(value string, n int) bool {
	return len(value) <= n
}
