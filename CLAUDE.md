# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Greenlight — a JSON API for a movie database, built from Alex Edwards'
"Let's Go" and "Let's Go Further" books, but backed by **SQLite**
(`modernc.org/sqlite`, pure Go, no cgo) instead of PostgreSQL. One Go binary,
one SQLite file, no external services required to run or test it.

Two documents already cover this codebase in depth — read them before making
non-trivial changes, don't re-derive what they already explain:

- **README.md** — full walkthrough: every PostgreSQL→SQLite difference (JSON
  columns for arrays, `COLLATE NOCASE` for citext, `LIKE` instead of FTS,
  connection PRAGMAs, typed error inspection...), design decisions (optimistic
  locking, sort safelisting, token scoping, input structs), and testing
  philosophy.
- **ARCHITECTURE.md** — condensed 30,000-foot view: the three-layer
  structure, request lifecycle, data model, auth flow, boot sequence.

## Commands

```bash
make run/seed          # create+migrate greenlight.db, seed demo user/movies, print an auth token
make run/api            # run the API server (auto-migrates on startup)

make test               # go test -race ./...
make test/short         # skip database-backed tests (go test -short)
make test/cover         # coverage (-coverpkg=./...), opens HTML report
make test/fuzz          # 30s per package across the 4 fuzz targets
make test/bench         # benchmarks with allocation counts
make audit               # tidy + fmt + vet + test -race  (run before finishing any change)

# Single test:
go test ./internal/data/ -run TestMovieModel_Insert -v
go test ./cmd/api/ -run TestAuthenticationAndPermissions -v

make db/shell            # sqlite3 shell against greenlight.db
make db/reset             # confirm; delete the db file, recreated fresh next run
make db/migrations/new name=add_foo   # requires the golang-migrate CLI (only for authoring; applying needs nothing extra)

make build/api            # build ./bin/api and ./bin/seed for current platform
make build/linux           # cross-compile linux/amd64, CGO_ENABLED=0

make help                  # list every target
```

Go 1.26+ is the only requirement — no Docker, no database server, no C
toolchain.

## Architecture, in brief

Three layers, request always flows top to bottom, nothing skips a layer:

```
cmd/api/        HTTP layer — routing, middleware, request/response JSON, auth enforcement
internal/data/  Model layer — domain types (Movie, User, Token, Permissions) + all SQL; knows nothing about HTTP
internal/db/    Storage — opens the SQLite file, applies embedded migrations on boot
migrations/     Versioned .sql schema, embedded via go:embed and applied automatically on startup
```

Supporting packages: `internal/validator` (field-error map), `internal/mailer`
(SMTP + embedded templates), `internal/testutil` (fresh migrated test DB per
test), `internal/assert` (test assertion helpers).

Every handler/middleware is a method on `*application` (`cmd/api/main.go`) —
there are no global variables, which is what lets each test build a fully
isolated app. The model layer returns only sentinel errors upward
(`ErrRecordNotFound`, `ErrEditConflict`); handlers translate those to HTTP
status codes. SQL details never leak past `internal/data`.

Middleware order in `routes.go` is deliberate and documented there —
`recoverPanic` wraps everything below it, `enableCORS` runs before
`rateLimit` so rejected requests still get CORS headers. Permission wrappers
compose: `requirePermission` → `requireActivatedUser` → `requireAuthenticatedUser`.

## Testing conventions

**`internal/testutil/doc.go` is the full guide to the suite** — commands,
layout, and the Go testing machinery used. Read it (`go doc
greenlight/internal/testutil`) before adding tests; don't re-derive it here.

- Every test gets its **own** SQLite database via `internal/testutil.NewDB(t)`
  — a temp file (not `:memory:`, which can vanish mid-test under connection
  pooling), migrated fresh from the embedded `.sql` files, cleaned up
  automatically. No mocks, no shared fixtures, safe to run in parallel. It takes
  a `testing.TB`, so benchmarks can use it too.
- `go test -short` skips anything database-backed.
- `cmd/api`, `cmd/seed` and `internal/data` each have a `TestMain` that drops
  `data.BcryptCost` to `bcrypt.MinCost` — full cost-12 bcrypt would make the
  suite take ~10x longer. This is safe because bcrypt embeds its cost factor
  in the hash itself, so verification is unaffected.
- `cmd/api` tests spin up a real `httptest` server; nothing is mocked except
  outbound email (`Mailer` is an interface for exactly this reason — see
  `cmd/api/main.go`). The real mailer is covered separately in
  `internal/mailer`, against a fake SMTP server defined in its own test file.
- **One test dependency: `github.com/google/go-cmp`**, used via
  `assert.DeepEqual` for slices/maps/structs that `assert.Equal`'s `comparable`
  constraint can't take. Don't add testify — `internal/assert/assert.go`
  documents why this project hand-rolls its helpers instead.
- Coverage baseline: `internal/validator` 100%, `internal/mailer` 93%,
  `internal/data` 91%, `internal/db` 84%, `cmd/api` 81%, `cmd/seed` 73%; **84%
  module-wide**. The rest is mostly `main`/`run` process wiring. Note
  `make test/cover` uses `-coverpkg=./...` — without it `internal/db` reads 0%,
  because it's exercised through `internal/testutil` from other packages.
- Beyond example-based tests there are **4 fuzz targets** (`Runtime.UnmarshalJSON`,
  `Genres.Scan`, `readJSON`, `EmailRX`), **concurrency tests** in
  `internal/data/concurrency_test.go` that guard `busy_timeout`/WAL/optimistic
  locking, and **benchmarks**. `testing/synctest` (stdlib, Go 1.25+) is used in
  `internal/mailer` to test the retry loop's sleeps in zero real time — it does
  not suit tests that go over a real socket, since bubble goroutines must be
  self-contained.

## Things worth knowing before touching SQL or migrations

- Placeholders are `?`, not `$1`. Genres are stored as JSON `TEXT` via a
  custom `Valuer`/`Scanner` (`internal/data/genres.go`) — SQLite has no array
  type. Email case-insensitivity comes from `COLLATE NOCASE`, not `citext`.
- **Foreign keys are OFF by default in SQLite** unless enabled per-connection.
  This project enables them via a DSN pragma in `internal/db/db.go` — don't
  remove it, `TestTokenModel_CascadeDeleteOnUser` guards against exactly that.
- PRAGMAs (`foreign_keys`, `journal_mode=WAL`, `busy_timeout`, `synchronous`)
  must live in the DSN, not in a one-off `PRAGMA` statement — `database/sql`
  pools connections, so a statement only configures whichever connection
  happens to run it.
- The `ORDER BY` column in list queries is interpolated (SQL doesn't allow
  placeholders for identifiers), so it's checked against a safelist the
  *handler* sets and panics on anything else (`Filters.sortColumn()`). Never
  build a sort column from unvalidated input.
- New migrations need matching `.up.sql`/`.down.sql` files created with the
  `golang-migrate` CLI (`make db/migrations/new name=...`); they're embedded
  into the binary and applied automatically at boot, so no manual migration
  step is needed to pick them up in tests or `make run/api`.
