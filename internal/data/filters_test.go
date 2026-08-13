package data

import (
	"testing"

	"greenlight/internal/assert"
	"greenlight/internal/validator"
)

// This test file lives in package `data` (not `data_test`) so it can reach the
// unexported sortColumn/sortDirection/limit/offset methods. Those are the ones
// that build SQL, so they're exactly what we want covered.

var testSafelist = []string{"id", "title", "year", "runtime", "-id", "-title", "-year", "-runtime"}

func TestFilters_sortColumn(t *testing.T) {
	tests := []struct {
		name string
		sort string
		want string
	}{
		{name: "ascending", sort: "title", want: "title"},
		// The leading '-' encodes direction and must be stripped from the
		// column name, or we'd generate `ORDER BY -title`.
		{name: "descending strips hyphen", sort: "-year", want: "year"},
		{name: "id", sort: "id", want: "id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Filters{Sort: tt.sort, SortSafelist: testSafelist}
			assert.Equal(t, f.sortColumn(), tt.want)
		})
	}
}

// TestFilters_sortColumnPanicsOnUnsafeValue verifies the SQL-injection guard.
//
// sortColumn is the one place a client-supplied string reaches the SQL text, so
// this test is a security regression test, not just a behaviour test. If
// someone ever "helpfully" replaces the panic with a default value, this fails.
func TestFilters_sortColumnPanicsOnUnsafeValue(t *testing.T) {
	// recover() only does anything inside a deferred function.
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected sortColumn to panic on a value outside the safelist, but it returned normally")
		}
	}()

	f := Filters{
		Sort:         "id; DROP TABLE movies--",
		SortSafelist: testSafelist,
	}

	f.sortColumn()
}

// TestFilters_sortDirection is the other half of the `-title` encoding: this
// method reads the hyphen that sortColumn above strips off.
//
// Splitting one query parameter across two methods like this is a small design
// wart — get them out of step and you'd sort by the right column in the wrong
// direction — so both are pinned. The empty case matters too: an absent ?sort=
// must default to ASC rather than producing `ORDER BY id ` with a dangling
// direction.
//
// Note these are written as one-liners rather than a table. There are only three
// cases and no setup, so a table would be more scaffolding than test.
func TestFilters_sortDirection(t *testing.T) {
	assert.Equal(t, Filters{Sort: "title"}.sortDirection(), "ASC")
	assert.Equal(t, Filters{Sort: "-title"}.sortDirection(), "DESC")
	assert.Equal(t, Filters{Sort: ""}.sortDirection(), "ASC")
}

// TestFilters_limitAndOffset covers the pagination arithmetic that becomes the
// query's LIMIT and OFFSET.
//
// The offset is where the off-by-one lives: page 1 must start at row 0, not row
// 20. Get it wrong by one page and every client silently skips or repeats a
// whole page of results — the kind of bug that's invisible until someone
// notices a missing record.
func TestFilters_limitAndOffset(t *testing.T) {
	tests := []struct {
		name       string
		page       int
		pageSize   int
		wantLimit  int
		wantOffset int
	}{
		{name: "first page", page: 1, pageSize: 20, wantLimit: 20, wantOffset: 0},
		{name: "second page", page: 2, pageSize: 20, wantLimit: 20, wantOffset: 20},
		{name: "tenth page small size", page: 10, pageSize: 5, wantLimit: 5, wantOffset: 45},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := Filters{Page: tt.page, PageSize: tt.pageSize}
			assert.Equal(t, f.limit(), tt.wantLimit)
			assert.Equal(t, f.offset(), tt.wantOffset)
		})
	}
}

func TestCalculateMetadata(t *testing.T) {
	tests := []struct {
		name         string
		totalRecords int
		page         int
		pageSize     int
		want         Metadata
	}{
		{
			name:         "no records returns empty metadata",
			totalRecords: 0, page: 1, pageSize: 20,
			// The zero Metadata serialises to `{}` thanks to the omitempty
			// tags — that's the intended behaviour for an empty result set.
			want: Metadata{},
		},
		{
			name:         "exactly one full page",
			totalRecords: 20, page: 1, pageSize: 20,
			want: Metadata{CurrentPage: 1, PageSize: 20, FirstPage: 1, LastPage: 1, TotalRecords: 20},
		},
		{
			name: "partial last page rounds up",
			// 13 records at 5 per page is 3 pages, not 2 — this is the
			// ceiling-division case.
			totalRecords: 13, page: 2, pageSize: 5,
			want: Metadata{CurrentPage: 2, PageSize: 5, FirstPage: 1, LastPage: 3, TotalRecords: 13},
		},
		{
			name:         "single record",
			totalRecords: 1, page: 1, pageSize: 20,
			want: Metadata{CurrentPage: 1, PageSize: 20, FirstPage: 1, LastPage: 1, TotalRecords: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Metadata is a struct of comparable fields, so == works and
			// assert.Equal can handle it directly.
			assert.Equal(t, calculateMetadata(tt.totalRecords, tt.page, tt.pageSize), tt.want)
		})
	}
}

func TestValidateFilters(t *testing.T) {
	tests := []struct {
		name      string
		filters   Filters
		wantValid bool
		wantKey   string // which field should have an error, if any
	}{
		{
			name:      "valid",
			filters:   Filters{Page: 1, PageSize: 20, Sort: "id", SortSafelist: testSafelist},
			wantValid: true,
		},
		{
			name:      "page zero",
			filters:   Filters{Page: 0, PageSize: 20, Sort: "id", SortSafelist: testSafelist},
			wantValid: false, wantKey: "page",
		},
		{
			name:      "page too large",
			filters:   Filters{Page: 10_000_001, PageSize: 20, Sort: "id", SortSafelist: testSafelist},
			wantValid: false, wantKey: "page",
		},
		{
			name:      "page size zero",
			filters:   Filters{Page: 1, PageSize: 0, Sort: "id", SortSafelist: testSafelist},
			wantValid: false, wantKey: "page_size",
		},
		{
			name:      "page size too large",
			filters:   Filters{Page: 1, PageSize: 101, Sort: "id", SortSafelist: testSafelist},
			wantValid: false, wantKey: "page_size",
		},
		{
			name:      "sort not in safelist",
			filters:   Filters{Page: 1, PageSize: 20, Sort: "password", SortSafelist: testSafelist},
			wantValid: false, wantKey: "sort",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := validator.New()
			ValidateFilters(v, tt.filters)

			assert.Equal(t, v.Valid(), tt.wantValid)

			if tt.wantKey != "" {
				if _, ok := v.Errors[tt.wantKey]; !ok {
					t.Errorf("expected a validation error on %q; got errors: %v", tt.wantKey, v.Errors)
				}
			}
		})
	}
}
