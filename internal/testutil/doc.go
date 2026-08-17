// Package testutil provides shared helpers for tests.
//
// The helper itself is in testdb.go. This file is the orientation guide for the
// whole test suite — what's where, how to run it, and the Go testing machinery
// it leans on. Read it with `go doc greenlight/internal/testutil`.
//
// # Running the tests
//
// The full command reference — test, test/short, test/cover, test/fuzz,
// test/bench, audit, and the rest — lives in AGENTS.md's Commands section, not
// repeated here, so the two lists can't drift apart. Short version: `mise run
// test` for the everyday run, `mise run test/short` to skip anything that
// touches a database.
//
// To run one test, or a group, pass a regular expression to -run:
//
//	go test ./internal/data/ -run TestMovieModel_Insert -v
//	go test ./cmd/api/ -run 'TestMovie' -v      # every test starting with TestMovie
//	go test ./... -run TestNothing              # matches nothing: a quick compile check
//
// -v prints each test name as it runs; without it you only see failures. -race
// enables the race detector, which is why `mise run test` is slower than a bare
// `go test` — it's worth it, and several tests here exist specifically to be run
// under it.
//
// # Where the tests live
//
// Go requires tests to sit beside the code they test, in files ending _test.go,
// which the compiler excludes from normal builds. So there's no separate tests/
// directory:
//
//	cmd/api/*_test.go          HTTP layer: handlers, middleware, routing, shutdown
//	cmd/seed/main_test.go      the dev seeding command
//	internal/data/*_test.go    model layer: SQL, validation, custom types
//	internal/db/db_test.go     connection setup and migrations
//	internal/mailer/*_test.go  templated email over a fake SMTP server
//	internal/validator/*_test.go   pure unit tests, no I/O at all
//
// Two files are infrastructure rather than tests:
//
//	internal/testutil    a fresh migrated database per test (this package)
//	internal/assert      three-and-a-bit assertion helpers
//
// # Unit tests vs integration tests
//
// Both are here, and the split is not the usual one.
//
// The pure unit tests are in internal/validator and the parts of internal/data
// that don't touch SQL (Runtime, Genres, Filters). They have no I/O and run in
// microseconds.
//
// Everything else is an integration test that talks to a REAL SQLite database
// with the REAL schema. That's unusual — most Go projects mock the database —
// and it's possible here only because SQLite is an embedded library: there's no
// server to start, so a "real database" is just a temp file that takes about a
// millisecond to create. Since the thing most likely to be wrong in a model
// layer is the SQL itself, and a mock cannot check SQL, this is a much better
// trade than it would be with Postgres.
//
// The cmd/api tests go one further and run the whole application behind an
// httptest.Server, driving it over real HTTP. Nothing is mocked except outbound
// email. See the harness in cmd/api/testutils_test.go.
//
// # Go testing machinery used throughout
//
// A tour of the pieces, since several are easy to miss:
//
//   - func TestXxx(t *testing.T) is the whole registration mechanism. There is
//     no annotation and no framework — a function whose name starts with Test
//     and takes *testing.T IS a test.
//
//   - t.Error/t.Errorf record a failure and CARRY ON. t.Fatal/t.Fatalf record it
//     and stop the test immediately. Use Fatal when continuing would just
//     produce a cascade of nil-pointer noise (a failed setup step), Error when
//     you'd like to see the other assertions too.
//
//   - t.Helper() marks a function as a helper, so a failure inside it is
//     reported at the CALLER's line. Without it every failure in internal/assert
//     would point at assert.go and tell you nothing. Every helper here calls it.
//
//   - t.Cleanup(fn) registers work to run when the test and all its subtests
//     finish, in last-registered-first order. It's preferable to defer inside a
//     helper, because a defer there would fire when the HELPER returns — long
//     before the test is done with what it set up.
//
//   - t.TempDir() creates a directory unique to the test and deletes it
//     afterwards. This is how every test gets its own database file.
//
//   - t.Run(name, fn) creates a subtest. It gives each case its own name in the
//     output and its own pass/fail, which is what makes the table-driven style
//     below readable when something breaks.
//
//   - testing.Short() reports whether -short was passed. Database-backed tests
//     check it and skip, so `mise run test/short` is a fast sanity check.
//
//   - TestMain(m *testing.M) runs once for the whole package, wrapping every
//     test in it. Three packages here use it to drop the bcrypt cost — see
//     internal/data/models_test.go for why that's safe.
//
// # The table-driven style
//
// The dominant pattern in Go, and used heavily here. Instead of writing ten
// near-identical test functions, declare the cases as a slice of structs and
// loop:
//
//	tests := []struct {
//		name string
//		body string
//		want string
//	}{
//		{"empty body", "", "body must not be empty"},
//		{"truncated", `{"a":`, "body contains badly-formed JSON"},
//	}
//
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			// ...one assertion, run once per case
//		})
//	}
//
// Adding a case is one line, and each gets its own name in the output. The
// anonymous struct type is declared inline because it's needed exactly once.
//
// # Fuzzing
//
// A few functions here take bytes straight off the wire and parse them by hand:
// Runtime.UnmarshalJSON, Genres.Scan, readJSON, and the email regexp. Those get
// fuzz targets — FuzzXxx(f *testing.F) — which the toolchain feeds mutated
// input for as long as you let it run.
//
// f.Add(...) supplies seed inputs to mutate from; good seeds are the tricky
// cases you already thought of. f.Fuzz(func(t *testing.T, data []byte){...}) is
// the body, and it should assert PROPERTIES ("never panics", "anything accepted
// round-trips unchanged") rather than specific outputs, because you don't know
// what input it'll be given.
//
// A normal `go test` run executes the seeds only, which is fast. Actual fuzzing
// needs -fuzz and takes as long as you allow:
//
//	go test ./internal/data/ -run=^$ -fuzz=FuzzGenresScan -fuzztime=30s
//
// The -run=^$ matters: it matches no ordinary test, so the run does nothing but
// fuzz. If a crasher is found it's written to that package's testdata/fuzz/
// directory and becomes a permanent seed — so the bug turns into a regression
// test automatically, and you commit the file.
//
// # Benchmarks
//
// BenchmarkXxx(b *testing.B), run with -bench. They don't run during `go test`.
//
//	go test ./internal/data/ -bench=. -benchmem -run=^$
//
// The loop is `for b.Loop() { ... }` (Go 1.24+), not the older
// `for i := 0; i < b.N; i++`. Besides reading better, b.Loop keeps the values it
// iterates over alive as far as the compiler is concerned, so the optimiser
// can't delete the work you're trying to measure — a real hazard when the
// operation is small.
//
// # The race detector
//
// -race instruments the binary to detect unsynchronised concurrent access. It
// finds real bugs that don't reproduce reliably otherwise, which is why
// `mise run test` always enables it.
//
// Worth understanding: it does NOT need two things to actually collide in wall
// time. It tracks happens-before relationships, and reports accesses it can't
// order — even if they were seconds apart. cmd/api/server_test.go has a comment
// on a case where that distinction shaped how the test had to be written.
//
// # testing/synctest
//
// New in the standard library as of Go 1.25. It runs code in a "bubble" with a
// FAKE clock: time only advances when every goroutine in the bubble is blocked,
// so a function that sleeps for a second finishes instantly and deterministically.
//
// Two places use it. internal/mailer/mailer_test.go tests a retry loop with two
// 500ms sleeps in zero real time. cmd/api/rate_limit_test.go tests the rate
// limiter's janitor, which sweeps once a minute and evicts after three — a
// four-minute test at real speed.
//
// The catch is that a bubble must be self-contained: goroutines started outside
// it are invisible, and one started inside that never exits FAILS the test with
// a deadlock panic. That rules it out for anything going over a real network
// socket, but it also turns out to be a feature — "did this goroutine shut
// down?" is otherwise an awkward thing to assert, and inside a bubble you get it
// for free. See TestRateLimitJanitorExitsOnShutdown, whose entire assertion is
// that the test finishes at all.
package testutil
