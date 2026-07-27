package data

import (
	"math"
	"strings"

	"greenlight/internal/validator"
)

// Filters holds the query-string parameters that control listing endpoints:
// which page, how big, and what to sort by.
//
// It's a separate type (rather than four loose arguments) so that the
// validation logic can live next to the data, and so handlers can pass a single
// value around.
type Filters struct {
	Page     int    // 1-based page number
	PageSize int    // records per page
	Sort     string // e.g. "title" or "-year" (leading '-' means descending)

	// SortSafelist is the set of values Sort is allowed to take. This is
	// populated by the handler, NOT by the client.
	//
	// This is a security control, not a convenience: the sort column gets
	// interpolated directly into the SQL string (you cannot parameterise an
	// ORDER BY column), so without a safelist this would be a SQL injection
	// hole. See sortColumn() below.
	SortSafelist []string
}

// sortColumn returns the database column to sort by, extracted from the Sort
// field.
//
// It PANICS if Sort isn't in the safelist. That looks alarming, but it's
// deliberate: ValidateFilters should already have rejected any unsafe value
// long before we get here, so reaching this panic means we have a bug that
// would otherwise become a SQL injection. Failing loudly beats failing quietly.
func (f Filters) sortColumn() string {
	for _, safeValue := range f.SortSafelist {
		if f.Sort == safeValue {
			// Strip any leading '-' — that encodes direction, not a column
			// name.
			return strings.TrimPrefix(f.Sort, "-")
		}
	}

	panic("unsafe sort parameter: " + f.Sort)
}

// sortDirection returns "ASC" or "DESC" depending on the prefix of the Sort
// field. A leading hyphen ("-year") means descending.
func (f Filters) sortDirection() string {
	if strings.HasPrefix(f.Sort, "-") {
		return "DESC"
	}

	return "ASC"
}

// limit returns the value for the SQL LIMIT clause.
func (f Filters) limit() int {
	return f.PageSize
}

// offset returns the value for the SQL OFFSET clause.
//
// Page 1 has offset 0, page 2 has offset PageSize, and so on.
//
// In theory (Page - 1) * PageSize could overflow an int, but ValidateFilters
// caps both values well below that, so we're safe.
func (f Filters) offset() int {
	return (f.Page - 1) * f.PageSize
}

// ValidateFilters checks that the pagination and sort parameters are sane.
func ValidateFilters(v *validator.Validator, f Filters) {
	v.Check(f.Page > 0, "page", "must be greater than zero")
	v.Check(f.Page <= 10_000_000, "page", "must be a maximum of 10 million")
	v.Check(f.PageSize > 0, "page_size", "must be greater than zero")
	v.Check(f.PageSize <= 100, "page_size", "must be a maximum of 100")

	// This is the check that keeps sortColumn()'s panic unreachable.
	v.Check(validator.PermittedValue(f.Sort, f.SortSafelist...), "sort", "invalid sort value")
}

// Metadata holds pagination information that we send back alongside a list of
// records, so clients can render "page 2 of 7" style UI without guessing.
//
// Every field is tagged with `omitempty` so that when there are no results at
// all we emit an empty object `{}` rather than a wall of zeroes.
type Metadata struct {
	CurrentPage  int `json:"current_page,omitempty"`
	PageSize     int `json:"page_size,omitempty"`
	FirstPage    int `json:"first_page,omitempty"`
	LastPage     int `json:"last_page,omitempty"`
	TotalRecords int `json:"total_records,omitempty"`
}

// calculateMetadata computes the pagination metadata from the total record
// count and the current page/page-size.
//
// When there are zero records we return an empty Metadata struct, which (thanks
// to the omitempty tags above) serialises as `{}`.
func calculateMetadata(totalRecords, page, pageSize int) Metadata {
	if totalRecords == 0 {
		return Metadata{}
	}

	return Metadata{
		CurrentPage: page,
		PageSize:    pageSize,
		FirstPage:   1,
		// Ceiling division: 13 records at 5 per page is 3 pages, not 2.
		// math.Ceil works on floats, so we convert in and back out.
		LastPage:     int(math.Ceil(float64(totalRecords) / float64(pageSize))),
		TotalRecords: totalRecords,
	}
}
