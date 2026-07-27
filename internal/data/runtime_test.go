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
				// errors.Is rather than == so the test still passes if the
				// error gets wrapped later.
				if !errors.Is(err, tt.wantError) {
					t.Fatalf("got error %v; want %v", err, tt.wantError)
				}
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
