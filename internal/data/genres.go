package data

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Genres holds the list of genres for a movie.
//
// ─────────────────────────────────────────────────────────────────────────────
// THIS FILE IS ONE OF THE MAIN POSTGRES → SQLITE DIFFERENCES.
// ─────────────────────────────────────────────────────────────────────────────
//
// In the book, the movies table has a `genres text[]` column — a native
// PostgreSQL array — and the pq driver ships a helper (pq.Array) to move Go
// slices in and out of it.
//
// SQLite has no array type. Its entire type system is five storage classes:
// NULL, INTEGER, REAL, TEXT and BLOB. So we have to encode the slice ourselves.
//
// We store it as a JSON string, i.e. the TEXT value `["drama","romance"]`.
// Why JSON rather than, say, a comma-separated list?
//
//  1. It's unambiguous. A genre containing a comma would break naive splitting.
//  2. SQLite has built-in JSON functions (json_each, json_array_length, ...),
//     so we can still query *inside* the column if we ever need to.
//  3. encoding/json does all the escaping work for us.
//
// The way we hook into database/sql is via two standard interfaces:
//
//   - driver.Valuer  — called when the value is used as a query ARGUMENT
//     (Go → database)
//   - sql.Scanner    — called when the value is read from a ROW
//     (database → Go)
//
// Implement those two methods and Genres can be passed to db.Exec and scanned
// from rows.Scan just like a string or an int, with zero special-casing at the
// call sites. That's the whole point: the ugliness is confined to this file.
//
// Note that Genres is a []string underneath, so it still marshals to a normal
// JSON array in our API responses — no MarshalJSON needed.
type Genres []string

// Value implements the driver.Valuer interface.
//
// database/sql calls this to convert Genres into something the driver can
// store. We hand back a string of JSON.
//
// The receiver is a value (not a pointer) so that both Genres and *Genres
// satisfy driver.Valuer.
func (g Genres) Value() (driver.Value, error) {
	// A nil slice would marshal to the JSON literal `null`. We'd rather store
	// an empty array, so that reading it back always yields a usable slice and
	// the column can stay NOT NULL.
	if g == nil {
		return "[]", nil
	}

	b, err := json.Marshal([]string(g))
	if err != nil {
		return nil, err
	}

	// driver.Value must be one of a small set of types: int64, float64, bool,
	// []byte, string, time.Time or nil. We return a string.
	return string(b), nil
}

// Scan implements the sql.Scanner interface.
//
// database/sql calls this with whatever the driver produced for the column.
// Depending on the driver and the column, that can arrive as a []byte or as a
// string — so we handle both rather than assuming.
func (g *Genres) Scan(src any) error {
	// A NULL column arrives as an untyped nil. Represent that as an empty
	// (non-nil) slice so callers never have to nil-check.
	if src == nil {
		*g = Genres{}
		return nil
	}

	var b []byte

	// A type switch is the idiomatic way to handle "one of several concrete
	// types". The `v :=` binds the value with its specific type in each case.
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		// Anything else means the column doesn't hold what we expect —
		// probably a schema/code mismatch, so fail loudly.
		return fmt.Errorf("data: cannot scan %T into Genres", src)
	}

	// An empty column is treated as an empty list rather than a JSON error.
	if len(b) == 0 {
		*g = Genres{}
		return nil
	}

	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("data: cannot unmarshal genres %q: %w", b, err)
	}

	// json.Unmarshal of `null` leaves out as nil; normalise to empty again.
	if out == nil {
		out = []string{}
	}

	*g = Genres(out)

	return nil
}
