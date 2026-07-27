package data

import (
	"encoding/json"
	"slices"
	"testing"

	"greenlight/internal/assert"
)

// The Genres type is the piece that replaces PostgreSQL's native text[], so it
// carries a lot of weight. These tests cover both directions and the awkward
// edge cases (nil, empty, NULL columns, values that need escaping).

func TestGenres_Value(t *testing.T) {
	tests := []struct {
		name   string
		genres Genres
		want   string
	}{
		{name: "typical", genres: Genres{"drama", "romance"}, want: `["drama","romance"]`},
		{name: "single", genres: Genres{"action"}, want: `["action"]`},

		// Both of these must produce `[]` rather than `null`, because the
		// column is NOT NULL and we want reads to always yield a usable slice.
		{name: "empty", genres: Genres{}, want: `[]`},
		{name: "nil", genres: nil, want: `[]`},

		// This is exactly why we store JSON rather than a comma-separated
		// string: encoding/json escapes the separator for us.
		{name: "value containing a comma", genres: Genres{"sci-fi, fantasy"}, want: `["sci-fi, fantasy"]`},
		{name: "value containing a quote", genres: Genres{`the "good" stuff`}, want: `["the \"good\" stuff"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.genres.Value()
			assert.NilError(t, err)

			// driver.Value is an interface; our implementation always returns
			// a string. The comma-ok assertion avoids a panic if that ever
			// changes.
			s, ok := got.(string)
			if !ok {
				t.Fatalf("got %T; want string", got)
			}

			assert.Equal(t, s, tt.want)
		})
	}
}

func TestGenres_Scan(t *testing.T) {
	tests := []struct {
		name      string
		src       any
		want      Genres
		wantError bool
	}{
		// The driver may hand us either a string or a []byte depending on the
		// column and the query, so both must work.
		{name: "from string", src: `["drama","romance"]`, want: Genres{"drama", "romance"}},
		{name: "from bytes", src: []byte(`["drama","romance"]`), want: Genres{"drama", "romance"}},

		{name: "empty array", src: `[]`, want: Genres{}},
		{name: "SQL NULL", src: nil, want: Genres{}},
		{name: "empty string", src: ``, want: Genres{}},
		{name: "JSON null", src: `null`, want: Genres{}},

		{name: "malformed json", src: `[drama`, wantError: true},
		{name: "wrong json type", src: `{"a":1}`, wantError: true},
		{name: "unsupported source type", src: 42, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Genres

			err := got.Scan(tt.src)

			if tt.wantError {
				if err == nil {
					t.Fatal("expected an error but got nil")
				}
				return
			}

			assert.NilError(t, err)

			// slices.Equal compares element by element. We can't use
			// assert.Equal here because slices aren't `comparable`.
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %#v; want %#v", got, tt.want)
			}

			// Scan must never produce a nil slice — callers rely on being able
			// to range over and len() the result without a nil check.
			if got == nil {
				t.Error("Scan produced a nil slice; want an empty one")
			}
		})
	}
}

func TestGenres_RoundTrip(t *testing.T) {
	original := Genres{"drama", "romance", "sci-fi, fantasy", `with "quotes"`}

	encoded, err := original.Value()
	assert.NilError(t, err)

	var decoded Genres
	err = decoded.Scan(encoded)
	assert.NilError(t, err)

	if !slices.Equal(decoded, original) {
		t.Errorf("got %#v; want %#v", decoded, original)
	}
}

// TestGenres_JSONRepresentation confirms that although Genres is stored as a
// JSON *string* in the database, it still appears as a normal JSON array in API
// responses — because its underlying type is []string and we did NOT give it a
// custom MarshalJSON.
//
// Getting this wrong (double-encoding) is a classic mistake with this pattern,
// so it's worth an explicit test.
func TestGenres_JSONRepresentation(t *testing.T) {
	b, err := json.Marshal(Genres{"drama", "romance"})
	assert.NilError(t, err)

	assert.Equal(t, string(b), `["drama","romance"]`)
}
