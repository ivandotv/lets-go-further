package validator

import (
	"testing"

	"greenlight/internal/assert"
)

// These are pure unit tests: no database, no network, no filesystem. They run
// in microseconds, which is why they're not gated behind testing.Short().

func TestValidator_CheckAndValid(t *testing.T) {
	v := New()

	// A brand-new validator has no errors.
	assert.Equal(t, v.Valid(), true)

	// A passing check records nothing.
	v.Check(true, "title", "must be provided")
	assert.Equal(t, v.Valid(), true)
	assert.Equal(t, len(v.Errors), 0)

	// A failing check records a message and flips Valid().
	v.Check(false, "title", "must be provided")
	assert.Equal(t, v.Valid(), false)
	assert.Equal(t, v.Errors["title"], "must be provided")
}

func TestValidator_AddErrorKeepsFirstMessage(t *testing.T) {
	// This documents a subtle behaviour that's easy to break by accident:
	// the FIRST message for a field wins. Checks are ordered
	// cheapest/most-fundamental first, so the first failure is the most
	// useful thing to show the user.
	v := New()

	v.AddError("email", "must be provided")
	v.AddError("email", "must be a valid email address")

	assert.Equal(t, v.Errors["email"], "must be provided")
	assert.Equal(t, len(v.Errors), 1)
}

// TestMatches is a table-driven test — the standard Go pattern.
//
// Each case is a struct in a slice, and t.Run creates a named SUBTEST for each.
// The benefits over a single test with many assertions:
//   - a failure names the specific case that broke
//   - every case runs even if an earlier one fails
//   - you can run one case with `go test -run TestMatches/invalid_no_at`
func TestMatches(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  bool
	}{
		{name: "valid simple", email: "alice@example.com", want: true},
		{name: "valid subdomain", email: "alice@mail.example.co.uk", want: true},
		{name: "valid plus addressing", email: "alice+movies@example.com", want: true},
		{name: "invalid empty", email: "", want: false},
		{name: "invalid no at", email: "alice.example.com", want: false},
		{name: "invalid no domain", email: "alice@", want: false},
		{name: "invalid no local part", email: "@example.com", want: false},
		{name: "invalid double at", email: "alice@@example.com", want: false},
		{name: "invalid space", email: "alice @example.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Matches(tt.email, EmailRX), tt.want)
		})
	}
}

func TestUnique(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "all distinct", values: []string{"a", "b", "c"}, want: true},
		{name: "has duplicate", values: []string{"a", "b", "a"}, want: false},
		{name: "empty slice", values: []string{}, want: true},
		{name: "nil slice", values: nil, want: true},
		{name: "single value", values: []string{"a"}, want: true},
		// Case matters — "Drama" and "drama" are different genres as far as
		// this check is concerned.
		{name: "differs only by case", values: []string{"Drama", "drama"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Unique(tt.values), tt.want)
		})
	}
}

func TestPermittedValue(t *testing.T) {
	safelist := []string{"id", "title", "-id", "-title"}

	assert.Equal(t, PermittedValue("title", safelist...), true)
	assert.Equal(t, PermittedValue("-title", safelist...), true)
	assert.Equal(t, PermittedValue("password", safelist...), false)
	assert.Equal(t, PermittedValue("", safelist...), false)

	// PermittedValue is generic, so it works on any comparable type.
	assert.Equal(t, PermittedValue(3, 1, 2, 3), true)
	assert.Equal(t, PermittedValue(9, 1, 2, 3), false)
}

func TestNotBlank(t *testing.T) {
	assert.Equal(t, NotBlank("hello"), true)
	assert.Equal(t, NotBlank(""), false)
	// The whole point of NotBlank over `!= ""`.
	assert.Equal(t, NotBlank("   "), false)
	assert.Equal(t, NotBlank("\t\n"), false)
}

func TestMaxBytes(t *testing.T) {
	assert.Equal(t, MaxBytes("hello", 5), true)
	assert.Equal(t, MaxBytes("hello", 4), false)

	// "é" is one character but TWO bytes in UTF-8. This test documents that
	// MaxBytes counts bytes, matching what the database column limits.
	assert.Equal(t, MaxBytes("é", 2), true)
	assert.Equal(t, MaxBytes("é", 1), false)
}
