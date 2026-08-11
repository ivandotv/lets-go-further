package data

import (
	"fmt"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"greenlight/internal/testutil"
)

// ─────────────────────────────────────────────────────────────────────────────
// BENCHMARKS
// ─────────────────────────────────────────────────────────────────────────────
//
// These aren't here to make anything faster. They're here as a tripwire and as
// documentation of two things that are easy to get wrong:
//
//   - What bcrypt actually costs, and therefore why the test suites override
//     BcryptCost in TestMain. Run BenchmarkPasswordSet and the ~250ms figure in
//     that comment stops being a claim and becomes a number on your machine.
//
//   - How MovieModel.GetAll scales with the filters applied to it. The title
//     filter uses LIKE rather than a real full-text index (see the README on
//     what was given up moving from Postgres), so it's a table scan — this is
//     where you'd see that start to matter.
//
// All of these use b.Loop() (Go 1.24+) rather than the older `for i := 0; i <
// b.N; i++` form. b.Loop is not just tidier: it keeps the values it iterates
// over alive from the compiler's point of view, so the optimiser can't delete
// the work being measured — which is a real hazard when benchmarking something
// as small as Genres.Value.
//
// Run them with:
//
//	go test ./internal/data/ -bench=. -benchmem -run=^$

func BenchmarkPasswordSet(b *testing.B) {
	// Deliberately NOT the reduced cost the tests use — the point is to measure
	// what production pays.
	original := BcryptCost
	BcryptCost = 12
	b.Cleanup(func() { BcryptCost = original })

	var p password

	for b.Loop() {
		if err := p.Set("pa55word1234"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPasswordMatches(b *testing.B) {
	original := BcryptCost
	BcryptCost = 12
	b.Cleanup(func() { BcryptCost = original })

	var p password
	if err := p.Set("pa55word1234"); err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		if _, err := p.Matches("pa55word1234"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPasswordMatchesMinCost measures the same operation at the cost the
// test suites use, which is what makes the TestMain override worth ~10x.
func BenchmarkPasswordMatchesMinCost(b *testing.B) {
	original := BcryptCost
	BcryptCost = bcrypt.MinCost
	b.Cleanup(func() { BcryptCost = original })

	var p password
	if err := p.Set("pa55word1234"); err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		if _, err := p.Matches("pa55word1234"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGenresValue(b *testing.B) {
	g := Genres{"drama", "romance", "war", "sci-fi", "adventure"}

	for b.Loop() {
		if _, err := g.Value(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGenresScan(b *testing.B) {
	src := []byte(`["drama","romance","war","sci-fi","adventure"]`)

	for b.Loop() {
		var g Genres
		if err := g.Scan(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRuntimeMarshalJSON(b *testing.B) {
	r := Runtime(107)

	for b.Loop() {
		if _, err := r.MarshalJSON(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRuntimeUnmarshalJSON(b *testing.B) {
	data := []byte(`"107 mins"`)

	for b.Loop() {
		var r Runtime
		if err := r.UnmarshalJSON(data); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMovieModelGetAll measures the list query against a populated table,
// with and without filters.
//
// The sub-benchmarks share one database, seeded once, so the setup cost isn't
// folded into the measurement.
func BenchmarkMovieModelGetAll(b *testing.B) {
	models := newBenchModels(b, 500)

	filters := Filters{
		Page: 1, PageSize: 20,
		Sort:         "id",
		SortSafelist: []string{"id", "title", "year", "-id", "-title", "-year"},
	}

	b.Run("unfiltered", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := models.Movies.GetAll("", nil, filters); err != nil {
				b.Fatal(err)
			}
		}
	})

	// The LIKE-based title search — the part that replaced Postgres full-text
	// search and doesn't use an index.
	b.Run("title filter", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := models.Movies.GetAll("movie 4", nil, filters); err != nil {
				b.Fatal(err)
			}
		}
	})

	// The genre filter goes through the JSON column, which is the other
	// SQLite-specific compromise worth watching.
	b.Run("genre filter", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := models.Movies.GetAll("", []string{"drama"}, filters); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("sorted by title", func(b *testing.B) {
		sorted := filters
		sorted.Sort = "-title"

		for b.Loop() {
			if _, _, err := models.Movies.GetAll("", nil, sorted); err != nil {
				b.Fatal(err)
			}
		}
	})

	// The deepest page, which is where LIMIT/OFFSET pagination gets expensive:
	// SQLite still has to walk every skipped row.
	b.Run("last page", func(b *testing.B) {
		deep := filters
		deep.Page = 25

		for b.Loop() {
			if _, _, err := models.Movies.GetAll("", nil, deep); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkMovieModelGet(b *testing.B) {
	models := newBenchModels(b, 500)

	for b.Loop() {
		if _, err := models.Movies.Get(250); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMovieModelInsert(b *testing.B) {
	models := newBenchModels(b, 0)

	for b.Loop() {
		movie := &Movie{
			Title:   "Benchmark",
			Year:    2020,
			Runtime: 100,
			Genres:  Genres{"drama"},
		}

		if err := models.Movies.Insert(movie); err != nil {
			b.Fatal(err)
		}
	}
}

// newBenchModels builds a database seeded with n movies.
//
// It mirrors newTestModels but takes a *testing.B, which works because
// testutil.NewDB accepts a testing.TB.
func newBenchModels(b *testing.B, n int) Models {
	b.Helper()

	models := NewModels(testutil.NewDB(b))

	genres := []Genres{
		{"drama"},
		{"action", "adventure"},
		{"comedy", "romance"},
		{"sci-fi"},
	}

	for i := range n {
		movie := &Movie{
			Title:   fmt.Sprintf("Movie %d", i),
			Year:    int32(1950 + i%70),
			Runtime: Runtime(80 + i%80),
			Genres:  genres[i%len(genres)],
		}

		if err := models.Movies.Insert(movie); err != nil {
			b.Fatal(err)
		}
	}

	// Don't count the seeding above.
	b.ResetTimer()

	return models
}
