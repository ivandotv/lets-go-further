package data

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"greenlight/internal/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// CONCURRENCY
// ─────────────────────────────────────────────────────────────────────────────
//
// Two things this project relies on are invisible in a single-threaded test:
//
//   - busy_timeout(5000) in the DSN. SQLite allows exactly ONE writer at a time
//     across the whole database. Without the pragma a second concurrent write
//     fails INSTANTLY with SQLITE_BUSY; with it, the driver waits for the lock.
//     Every test elsewhere in this package writes from one goroutine, so none of
//     them would notice if that pragma disappeared.
//
//   - Optimistic locking (the `AND version = ?` clause in MovieModel.Update).
//     TestMovieModel_UpdateEditConflict simulates a conflict by holding a stale
//     copy; these tests produce a real one from two goroutines racing.
//
// Run under `go test -race`, which `mise run test` does.
//
// ── THE CONCURRENCY PATTERN USED BELOW ───────────────────────────────────────
//
// Every test in this file follows the same shape, and it's worth reading once:
//
//	var (
//		wg   sync.WaitGroup   // counts outstanding goroutines
//		mu   sync.Mutex       // guards the shared results slice
//		errs []error          // collected from all goroutines
//	)
//
//	for i := range n {
//		wg.Add(1)                        // BEFORE starting the goroutine
//		go func() {
//			defer wg.Done()          // deferred, so a panic still decrements
//			...
//			mu.Lock()
//			errs = append(errs, err) // append is NOT safe concurrently
//			mu.Unlock()
//		}()
//	}
//	wg.Wait()                                // block until all are done
//
// Three things that are easy to get wrong:
//
//   - wg.Add(1) must happen in the LOOP, not inside the goroutine. Inside,
//     Wait() could run before any Add did and return immediately.
//
//   - The mutex is required because a slice append can reallocate the backing
//     array, and two goroutines doing that at once corrupt it. Running without
//     -race, that usually looks like results mysteriously going missing.
//
//   - Results are collected and asserted AFTER Wait(), never inside the
//     goroutines. t.Errorf is safe to call concurrently, but t.Fatalf is not —
//     it calls runtime.Goexit, which only ends the goroutine that called it and
//     would leave the test hanging on Wait().
//
// One more, specific to modern Go: the goroutines below close over the loop
// variable `i` directly, without passing it as an argument. That is only correct
// as of Go 1.22, which gives each iteration its OWN copy of the variable. In
// earlier versions all iterations shared one, and every goroutine would likely
// see the final value — the single most common Go bug there was. This module
// requires Go 1.26, so the modern behaviour applies.

// TestConcurrentInsertsAllSucceed is the busy_timeout guard.
//
// Every one of these inserts contends for the same database-level write lock.
// If the pragma were removed from buildDSN, most of them would fail
// immediately with "database is locked" and this test would say so loudly.
func TestConcurrentInsertsAllSucceed(t *testing.T) {
	models := newTestModels(t)

	const writers = 20

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for i := range writers {
		wg.Add(1)

		go func() {
			// Deferred so it runs even if something below panics — otherwise a
			// single panicking goroutine would leave Wait() blocked forever and
			// the test would time out with a confusing stack dump.
			defer wg.Done()

			movie := &Movie{
				Title:   fmt.Sprintf("Concurrent %d", i),
				Year:    2000 + int32(i),
				Runtime: 100,
				Genres:  Genres{"drama"},
			}

			if err := models.Movies.Insert(movie); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("writer %d: %w", i, err))
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent insert failed: %v", err)
	}
	if len(errs) > 0 {
		t.Fatal("concurrent writes failed; is busy_timeout still set in internal/db.buildDSN?")
	}

	// Every insert must have landed, with a distinct ID.
	movies, _, err := models.Movies.GetAll("", nil, Filters{
		Page: 1, PageSize: 100, Sort: "id", SortSafelist: []string{"id"},
	})
	assert.NilError(t, err)
	assert.Equal(t, len(movies), writers)

	seen := make(map[int64]bool, writers)
	for _, m := range movies {
		if seen[m.ID] {
			t.Errorf("duplicate movie ID %d; the ID sequence is not safe under concurrency", m.ID)
		}
		seen[m.ID] = true
	}
}

// TestConcurrentUpdatesExactlyOneWins produces a genuine edit conflict.
//
// Both goroutines read version 1 and then try to write it. The optimistic-lock
// clause means the first UPDATE to reach the database bumps the version to 2,
// so the second matches zero rows and comes back as ErrEditConflict.
//
// The important assertion is not "one failed" but "exactly one SUCCEEDED" — a
// lost update (both reporting success, one silently overwriting the other) is
// the bug this design exists to prevent, and it would be invisible without
// counting.
func TestConcurrentUpdatesExactlyOneWins(t *testing.T) {
	models := newTestModels(t)

	original := &Movie{
		Title:   "Contested",
		Year:    1999,
		Runtime: 120,
		Genres:  Genres{"drama"},
	}
	assert.NilError(t, models.Movies.Insert(original))

	// Both goroutines read the record independently, at version 1.
	first, err := models.Movies.Get(original.ID)
	assert.NilError(t, err)
	second, err := models.Movies.Get(original.ID)
	assert.NilError(t, err)

	assert.Equal(t, first.Version, second.Version)

	first.Title = "Written by A"
	second.Title = "Written by B"

	// A barrier, so both writes are attempted as close together as possible.
	//
	// The pattern: every goroutine blocks on `<-start`, and closing the channel
	// releases them all at once. Closing a channel makes every receive on it
	// return immediately — which is why `chan struct{}` closed as a broadcast is
	// the idiomatic Go way to start N goroutines simultaneously. (struct{} is a
	// zero-byte type: no value is ever sent, the close IS the signal.)
	//
	// Without it, the first goroutine would usually finish before the second
	// started, and the conflict this test exists to produce would never happen.
	start := make(chan struct{})

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		results  []error
		attempts = []*Movie{first, second}
	)

	for _, movie := range attempts {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			err := models.Movies.Update(movie)

			mu.Lock()
			results = append(results, err)
			mu.Unlock()
		}()
	}

	close(start)
	wg.Wait()

	var succeeded, conflicted int

	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrEditConflict):
			conflicted++
		default:
			t.Errorf("unexpected error from a concurrent update: %v", err)
		}
	}

	assert.Equal(t, succeeded, 1)
	assert.Equal(t, conflicted, 1)

	// And the winner's write is what's actually stored, at version 2.
	final, err := models.Movies.Get(original.ID)
	assert.NilError(t, err)

	assert.Equal(t, final.Version, int32(2))

	if final.Title != "Written by A" && final.Title != "Written by B" {
		t.Errorf("got title %q; want one of the two concurrent writes", final.Title)
	}
}

// TestConcurrentReadsDuringWrite exercises the WAL journal mode.
//
// Under SQLite's default rollback journal a writer BLOCKS all readers. Under
// WAL — which buildDSN requests — readers see the last committed snapshot and
// proceed in parallel. For a web server that difference is the whole ballgame,
// and this test fails (by timing out on the reads) if journal_mode is lost.
func TestConcurrentReadsDuringWrite(t *testing.T) {
	models := newTestModels(t)

	// Seed something to read.
	for i := range 5 {
		movie := &Movie{
			Title:   fmt.Sprintf("Seed %d", i),
			Year:    1990 + int32(i),
			Runtime: 100,
			Genres:  Genres{"drama"},
		}
		assert.NilError(t, models.Movies.Insert(movie))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		filters = Filters{Page: 1, PageSize: 20, Sort: "id", SortSafelist: []string{"id"}}
	)

	// Writers and readers at the same time.
	for i := range 10 {
		wg.Add(2)

		go func() {
			defer wg.Done()

			movie := &Movie{
				Title:   fmt.Sprintf("Writer %d", i),
				Year:    2010 + int32(i),
				Runtime: 100,
				Genres:  Genres{"action"},
			}

			if err := models.Movies.Insert(movie); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("write %d: %w", i, err))
				mu.Unlock()
			}
		}()

		go func() {
			defer wg.Done()

			// Reads must never fail, and must always see a consistent snapshot
			// — never a partially-applied write.
			movies, _, err := models.Movies.GetAll("", nil, filters)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("read %d: %w", i, err))
				mu.Unlock()
				return
			}

			for _, m := range movies {
				if m.Title == "" || m.Genres == nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("read %d saw a torn row: %+v", i, m))
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("%v", err)
	}
}

// TestConcurrentTokenCreation checks the token path under load.
//
// Tokens are generated with crypto/rand and stored with a UNIQUE hash. If
// generateToken's entropy were reduced — a plausible "simplification" — this
// would start failing with a uniqueness violation rather than silently making
// tokens guessable.
func TestConcurrentTokenCreation(t *testing.T) {
	models := newTestModels(t)

	user := newTestUser(t, models.Users, "Concurrent", "concurrent@example.com", "pa55word1234")

	const tokens = 25

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		issued []string
		errs   []error
	)

	for range tokens {
		wg.Add(1)

		go func() {
			defer wg.Done()

			token, err := models.Tokens.New(user.ID, 24*time.Hour, ScopeAuthentication)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			mu.Lock()
			issued = append(issued, token.Plaintext)
			mu.Unlock()
		}()
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent token creation failed: %v", err)
	}

	assert.Equal(t, len(issued), tokens)

	// Every token must be distinct.
	seen := make(map[string]bool, len(issued))
	for _, plaintext := range issued {
		if seen[plaintext] {
			t.Fatalf("generateToken produced a duplicate token %q; entropy is compromised", plaintext)
		}
		seen[plaintext] = true
	}
}
