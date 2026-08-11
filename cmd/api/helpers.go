package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"greenlight/internal/validator"
)

// envelope wraps every JSON response in a single top-level object.
//
// So instead of returning a bare movie:
//
//	{"id": 1, "title": "Casablanca"}
//
// we return:
//
//	{"movie": {"id": 1, "title": "Casablanca"}}
//
// Three reasons this is worth the extra nesting:
//
//  1. The response is self-describing — you can tell what you're looking at.
//  2. There's room to add metadata later without breaking clients.
//  3. It defuses a (historical) JSON hijacking attack that relied on
//     top-level arrays being valid JavaScript.
//
// It's `map[string]any` so any handler can use it without declaring a type.
type envelope map[string]any

// readIDParam extracts the "{id}" URL wildcard and validates it.
//
// http.ServeMux stores matched wildcards on the request itself; this helper
// hides that detail (and the string→int conversion) from every handler that
// needs an ID.
func (app *application) readIDParam(r *http.Request) (int64, error) {
	// Base 10, 64 bits — matching the int64 our IDs are stored as.
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid id parameter")
	}

	return id, nil
}

// writeJSON serialises data to JSON and writes it to the response.
//
// Returning an error (rather than writing it out itself) lets the caller decide
// how to react — which matters because by the time marshalling fails we may
// not have written anything yet, and can still send a 500.
func (app *application) writeJSON(w http.ResponseWriter, status int, data envelope, headers http.Header) error {
	// MarshalIndent instead of Marshal purely for human readability when
	// poking at the API with curl. It costs a little CPU and a few extra
	// bytes; for a learning project the trade is worth it.
	js, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		return err
	}

	// A trailing newline makes terminal output much nicer.
	js = append(js, '\n')

	// Copy any extra headers in. Ranging over a nil map is legal in Go and
	// simply does nothing, so callers can pass nil.
	for key, value := range headers {
		w.Header()[key] = value
	}

	w.Header().Set("Content-Type", "application/json")

	// ORDER MATTERS: every header must be set before WriteHeader is called.
	// After that the status line has gone out and header changes are ignored.
	w.WriteHeader(status)
	w.Write(js)

	return nil
}

// readJSON decodes a JSON request body into dst, converting the standard
// library's decoding errors into messages that are actually useful to an API
// client.
//
// Out of the box, a malformed body produces errors like
// "json: cannot unmarshal number into Go struct field ...", which leaks our
// internal type names and reads like a stack trace. This function triages the
// error types and produces something a client can act on.
func (app *application) readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	// Cap the request body at 1MB. Without this, a client could send a
	// multi-gigabyte body and exhaust the server's memory. MaxBytesReader also
	// closes the underlying reader, unlike io.LimitReader.
	maxBytes := 1_048_576
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxBytes))

	dec := json.NewDecoder(r.Body)

	// Reject fields in the JSON that don't map to a struct field. Without this,
	// a typo like {"titel": "..."} would be silently ignored and the client
	// would be baffled as to why their update did nothing.
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err != nil {
		// These are the concrete error types Decode can return. We declare
		// variables of each so errors.As can match against them.
		var syntaxError *json.SyntaxError
		var unmarshalTypeError *json.UnmarshalTypeError
		var invalidUnmarshalError *json.InvalidUnmarshalError
		var maxBytesError *http.MaxBytesError

		switch {
		// Badly-formed JSON at a known byte offset.
		case errors.As(err, &syntaxError):
			return fmt.Errorf("body contains badly-formed JSON (at character %d)", syntaxError.Offset)

		// Badly-formed JSON that the decoder hit at EOF. There's a known
		// wrinkle where this surfaces as io.ErrUnexpectedEOF rather than a
		// SyntaxError, so we check for it separately.
		case errors.Is(err, io.ErrUnexpectedEOF):
			return errors.New("body contains badly-formed JSON")

		// Right JSON, wrong type — e.g. a string where we wanted a number.
		case errors.As(err, &unmarshalTypeError):
			if unmarshalTypeError.Field != "" {
				return fmt.Errorf("body contains incorrect JSON type for field %q", unmarshalTypeError.Field)
			}
			return fmt.Errorf("body contains incorrect JSON type (at character %d)", unmarshalTypeError.Offset)

		// Empty body.
		case errors.Is(err, io.EOF):
			return errors.New("body must not be empty")

		// DisallowUnknownFields triggered. The standard library doesn't give us
		// a distinct error type for this, so string matching is the only
		// option — we extract the field name from the message.
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			fieldName := strings.TrimPrefix(err.Error(), "json: unknown field ")
			return fmt.Errorf("body contains unknown key %s", fieldName)

		// Body exceeded MaxBytesReader's limit.
		case errors.As(err, &maxBytesError):
			return fmt.Errorf("body must not be larger than %d bytes", maxBytesError.Limit)

		// This one means WE passed something invalid (a non-pointer) to
		// Decode. That's a programming bug, not bad input, so panic rather
		// than returning it to the client.
		case errors.As(err, &invalidUnmarshalError):
			panic(err)

		default:
			return err
		}
	}

	// ── The second Decode call ───────────────────────────────────────────────
	//
	// A JSON decoder will happily read the FIRST value in the stream and stop.
	// So a body of `{"title":"a"} {"title":"b"}` would decode the first object
	// and silently discard the second.
	//
	// Calling Decode again should return io.EOF if the body held exactly one
	// value. Anything else means there was trailing content.
	//
	// struct{}{} is a zero-byte throwaway destination — we only care about the
	// error.
	err = dec.Decode(&struct{}{})
	if !errors.Is(err, io.EOF) {
		return errors.New("body must only contain a single JSON value")
	}

	return nil
}

// ── Query string helpers ─────────────────────────────────────────────────────
//
// These three read values out of the URL query string, falling back to a
// default when the parameter is absent. They take the already-parsed url.Values
// so a handler parses the query string only once.

// readString returns a string value from the query string, or defaultValue if
// the key is absent or empty.
func (app *application) readString(qs url.Values, key string, defaultValue string) string {
	s := qs.Get(key)
	if s == "" {
		return defaultValue
	}

	return s
}

// readCSV reads a comma-separated parameter into a slice.
//
// Used for `?genres=drama,romance`.
func (app *application) readCSV(qs url.Values, key string, defaultValue []string) []string {
	csv := qs.Get(key)
	if csv == "" {
		return defaultValue
	}

	return strings.Split(csv, ",")
}

// readInt reads an integer parameter, recording a validation error if the value
// isn't a valid integer.
//
// The Validator is passed in (rather than returning an error) so that a bad
// `?page=abc` joins all the other validation failures in a single 422 response,
// instead of short-circuiting.
func (app *application) readInt(qs url.Values, key string, defaultValue int, v *validator.Validator) int {
	s := qs.Get(key)
	if s == "" {
		return defaultValue
	}

	i, err := strconv.Atoi(s)
	if err != nil {
		v.AddError(key, "must be an integer value")
		return defaultValue
	}

	return i
}
