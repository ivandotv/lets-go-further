package data

import (
	"encoding/json"
	"errors"
	"testing"

	"greenlight/internal/assert"
)

func TestRuntime_MarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		runtime Runtime
		want    string
	}{
		{name: "typical", runtime: Runtime(102), want: `"102 mins"`},
		{name: "zero", runtime: Runtime(0), want: `"0 mins"`},
		{name: "large", runtime: Runtime(1440), want: `"1440 mins"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.runtime)
			assert.NilError(t, err)
			assert.Equal(t, string(got), tt.want)
		})
	}
}

// TestRuntime_MarshalJSONViaStruct checks that the custom marshaller is picked
// up when a Runtime is a struct FIELD, not just when marshalled directly.
//
// This is the case that actually matters in production, and it's the one that
// silently breaks if MarshalJSON is defined on a pointer receiver instead of a
// value receiver.
func TestRuntime_MarshalJSONViaStruct(t *testing.T) {
	s := struct {
		Runtime Runtime `json:"runtime"`
	}{Runtime: 107}

	got, err := json.Marshal(s)
	assert.NilError(t, err)
	assert.Equal(t, string(got), `{"runtime":"107 mins"}`)
}

func TestRuntime_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      Runtime
		wantError error
	}{
		{name: "valid", input: `"102 mins"`, want: 102},
		{name: "valid zero", input: `"0 mins"`, want: 0},

		// Every one of these should produce ErrInvalidRuntimeFormat rather
		// than a raw encoding/json error, because the handler layer checks for
		// that specific sentinel to produce a friendly 422.
		{name: "bare number", input: `102`, wantError: ErrInvalidRuntimeFormat},
		{name: "missing unit", input: `"102"`, wantError: ErrInvalidRuntimeFormat},
		{name: "wrong unit", input: `"102 minutes"`, wantError: ErrInvalidRuntimeFormat},
		{name: "not a number", input: `"abc mins"`, wantError: ErrInvalidRuntimeFormat},
		{name: "too many parts", input: `"102 mins long"`, wantError: ErrInvalidRuntimeFormat},
		{name: "empty string", input: `""`, wantError: ErrInvalidRuntimeFormat},
		{name: "json object", input: `{}`, wantError: ErrInvalidRuntimeFormat},
		{name: "json null", input: `null`, wantError: ErrInvalidRuntimeFormat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runtime Runtime

			err := runtime.UnmarshalJSON([]byte(tt.input))

			if tt.wantError != nil {
				assert.ErrorIs(t, err, tt.wantError)
				return
			}

			assert.NilError(t, err)
			assert.Equal(t, runtime, tt.want)
		})
	}
}

// TestRuntime_RoundTrip checks marshal→unmarshal returns the original value.
//
// Round-trip tests are worth writing for any custom codec: they catch the whole
// class of bugs where the two halves drift apart, without you having to think
// up which specific values might break.
func TestRuntime_RoundTrip(t *testing.T) {
	for _, original := range []Runtime{0, 1, 42, 102, 32767, 1000000} {
		encoded, err := json.Marshal(original)
		assert.NilError(t, err)

		var decoded Runtime
		err = json.Unmarshal(encoded, &decoded)
		assert.NilError(t, err)

		assert.Equal(t, decoded, original)
	}
}

// FuzzRuntimeUnmarshalJSON checks two properties across arbitrary input.
//
// Fuzzing suits this function particularly well: it's a hand-written parser
// that does string surgery (Unquote, Split, ParseInt) on attacker-controlled
// bytes, and every one of those steps has edge cases a table-driven test only
// samples. The table above covers the cases we THOUGHT of; this covers the ones
// we didn't.
//
// The two properties:
//
//  1. It never panics. An index-out-of-range here would be a remote DoS, since
//     this runs on request bodies before any authentication.
//  2. Round-trip stability: any input it ACCEPTS must survive a
//     marshal/unmarshal cycle unchanged. That's what stops a "fix" to one
//     method from silently desynchronising it from the other.
func FuzzRuntimeUnmarshalJSON(f *testing.F) {
	// Seeds are the interesting shapes from the table test above, plus the
	// boundary values of the int32 range the parser targets.
	seeds := []string{
		`"107 mins"`,
		`"0 mins"`,
		`"-1 mins"`,
		`"2147483647 mins"`,  // math.MaxInt32
		`"-2147483648 mins"`, // math.MinInt32
		`"2147483648 mins"`,  // one past MaxInt32; must be rejected
		`"107"`,
		`"107 minutes"`,
		`"107 mins extra"`,
		`" 107 mins"`,
		`""`,
		`107`,
		`null`,
		`{}`,
		`"mins mins"`,
		`"+107 mins"`,
	}

	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var r Runtime

		// Property 1: no panic, whatever the input.
		if err := r.UnmarshalJSON(data); err != nil {
			// Every rejection must use the sentinel the handlers switch on. If
			// this ever returned a bare error, createMovieHandler would turn a
			// bad runtime into a 500 instead of a 422.
			if !errors.Is(err, ErrInvalidRuntimeFormat) {
				t.Fatalf("UnmarshalJSON(%q) returned %v; want ErrInvalidRuntimeFormat", data, err)
			}
			return
		}

		// Property 2: accepted input round-trips.
		marshalled, err := r.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON after accepting %q: %v", data, err)
		}

		var got Runtime
		if err := got.UnmarshalJSON(marshalled); err != nil {
			t.Fatalf("input %q accepted as %d, but its own output %q was rejected: %v",
				data, r, marshalled, err)
		}

		if got != r {
			t.Errorf("round-trip changed the value: %q -> %d -> %q -> %d",
				data, r, marshalled, got)
		}
	})
}
