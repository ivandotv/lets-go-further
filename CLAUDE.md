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

Tasks live in `mise.toml` and run via [mise](https://mise.jdx.dev/) — there is
no Makefile. Task names are unchanged from the Makefile that preceded it, so
every command is `mise run <name>`.

```bash
mise run run/seed       # create+migrate greenlight.db, seed demo user/movies, print an auth token
mise run run/api        # run the API server (auto-migrates on startup)

mise run test           # go test -race ./...
mise run test/short     # skip database-backed tests (go test -short)
mise run test/cover     # coverage (-coverpkg=./...), opens HTML report
mise run test/fuzz      # 30s per target across the 4 fuzz targets
mise run test/bench     # benchmarks with allocation counts
mise run audit          # tidy + fmt + vet + test -race  (run before finishing any change)

# Single test:
go test ./internal/data/ -run TestMovieModel_Insert -v
go test ./cmd/api/ -run TestAuthenticationAndPermissions -v

mise run db/shell            # sqlite3 shell against greenlight.db
mise run db/reset            # prompts; delete the db file, recreated fresh next run
mise run db/migrations/new add_foo   # requires the golang-migrate CLI (only for authoring; applying needs nothing extra)

mise run build/api      # build ./bin/api and ./bin/seed for current platform
mise run build/linux    # cross-compile linux/amd64, CGO_ENABLED=0

mise tasks              # list every task with its description
```

Go 1.26+ is the only requirement — no Docker, no database server, no C
toolchain. (mise also pins the Go version via `[tools]`, but nothing in the
project *requires* mise — every task is a plain `go` command you can read out
of `mise.toml` and run directly.)

Four differences from the old Makefile, worth remembering when editing tasks:

- **Variables are env vars, not `make x y=z`**: `db_dsn=/tmp/other.db mise run
  run/api`, `fuzztime=10s mise run test/fuzz`. Defaults live in `[vars]`, each
  written as `{{ env.x | default(value='...') }}`.
- **`db/migrations/new` takes a positional arg**, not `name=`. mise refuses to
  run it without one.
- **`db/reset` and `db/migrations/down` use `confirm`**, which needs a real
  TTY — piping `y` in won't work. Non-interactive: `mise run --yes db/reset`.
- **`mise trust` is required once** per clone before tasks will run.

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
  `mise run test/cover` uses `-coverpkg=./...` — without it `internal/db` reads 0%,
  because it's exercised through `internal/testutil` from other packages.
- Beyond example-based tests there are **4 fuzz targets** (`Runtime.UnmarshalJSON`,
  `Genres.Scan`, `readJSON`, `EmailRX`), **concurrency tests** in
  `internal/data/concurrency_test.go` that guard `busy_timeout`/WAL/optimistic
  locking, and **benchmarks**. `testing/synctest` (stdlib, Go 1.25+) is used in
  `internal/mailer` for the retry loop's sleeps and in `cmd/api` for the rate
  limiter's janitor — it does not suit tests that go over a real socket, since
  bubble goroutines must be self-contained.
- Long-lived background loops started by middleware select on `app.shutdown`
  and exit when `app.stop()` closes it. `run()` initialises the channel and
  `serve()` calls `stop()`; `newTestApplication` does both via `t.Cleanup`.
  A nil `shutdown` means "never stop", so hand-built `&application{}` literals
  in tests stay valid. Don't reintroduce a `for { time.Sleep(...) }` loop with
  no exit — `TestRateLimitJanitorExitsOnShutdown` fails the whole binary if you
  do, and it would block any future use of synctest in this package.

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
  `golang-migrate` CLI (`mise run db/migrations/new add_foo`); they're embedded
  into the binary and applied automatically at boot, so no manual migration
  step is needed to pick them up in tests or `mise run run/api`.
