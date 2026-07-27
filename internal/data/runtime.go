package data

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidRuntimeFormat is returned by Runtime.UnmarshalJSON when the incoming
// JSON value isn't in the shape we expect. The handler layer checks for this
// specific error so it can turn it into a friendly 422 response rather than a
// generic 400.
var ErrInvalidRuntimeFormat = errors.New("invalid runtime format")

// Runtime holds a movie's length in minutes.
//
// It's defined as a named type with int32 as its *underlying* type. That means
// it behaves like an int32 for arithmetic and comparison, but — crucially — we
// can hang our own methods off it. Here we use that to control exactly how a
// runtime is represented in JSON.
//
// In the database it's a plain integer. In JSON we want it to look like
// "102 mins" instead of a bare 102, so the API is self-describing.
type Runtime int32

// MarshalJSON implements the json.Marshaler interface.
//
// Because we define it on the VALUE receiver (Runtime, not *Runtime), it gets
// picked up whether the encoder is handed a Runtime or a *Runtime. If we'd used
// a pointer receiver, encoding a non-addressable Runtime value would silently
// fall back to the default int encoding — a classic Go gotcha.
func (r Runtime) MarshalJSON() ([]byte, error) {
	// Build the string we want to appear in the output, e.g. `102 mins`.
	jsonValue := fmt.Sprintf("%d mins", r)

	// A MarshalJSON method must return *valid JSON*. A bare `102 mins` is not
	// valid JSON — it needs to be a quoted string. strconv.Quote wraps it in
	// double quotes and escapes anything that needs escaping.
	quotedJSONValue := strconv.Quote(jsonValue)

	return []byte(quotedJSONValue), nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
//
// This is the inverse of MarshalJSON: it takes `"102 mins"` from a request body
// and turns it back into Runtime(102).
//
// Note that this one MUST use a pointer receiver. We're modifying the value the
// receiver points at, so a value receiver would just mutate a copy and throw it
// away.
func (r *Runtime) UnmarshalJSON(jsonValue []byte) error {
	// The incoming value should be a quoted string like `"102 mins"` (with the
	// double quotes included in the byte slice). Strip the quotes; if that
	// fails, the client sent us something in the wrong shape — a number, an
	// object, whatever — so return our sentinel error.
	unquotedJSONValue, err := strconv.Unquote(string(jsonValue))
	if err != nil {
		return ErrInvalidRuntimeFormat
	}

	// Split on the space to isolate the number from the unit.
	parts := strings.Split(unquotedJSONValue, " ")

	// Sanity-check the parts: we want exactly two, and the second must be
	// "mins".
	if len(parts) != 2 || parts[1] != "mins" {
		return ErrInvalidRuntimeFormat
	}

	// Parse the number. Base 10, 32 bits wide (to match Runtime's underlying
	// type).
	i, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil {
		return ErrInvalidRuntimeFormat
	}

	// Convert to Runtime and assign through the pointer. The * dereferences the
	// receiver so we're writing to the caller's value, not our local copy.
	*r = Runtime(i)

	return nil
}
