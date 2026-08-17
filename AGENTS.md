# AGENTS.md

This file provides guidance to AI coding agents (Claude Code, and others that
follow the [AGENTS.md](https://agents.md) convention) when working with code
in this repository.

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
mise run test/bench/compare old.txt new.txt  # benchstat diff between two runs
mise run test/summary   # tests via gotestsum, readable pass/fail output
mise run audit          # tidy + fmt + vet + test -race  (run before finishing any change)
mise run lint           # golangci-lint (vet, staticcheck, errcheck, ...)
mise run vuln           # govulncheck against the Go vulnerability database

# test/summary, test/bench/compare, lint, and vuln each wrap a third-party CLI
# (gotestsum, benchstat, golangci-lint, govulncheck) that isn't a module
# dependency. Unlike golang-migrate below (manual install), these four are in
# mise.toml's [tools], so `mise install` fetches them automatically. Not part
# of `audit`, which stays dependency-free on purpose.

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

- **Variables are env vars, not `make x y=z`**: `fuzztime=10s mise run
  test/fuzz`. Defaults live in `[vars]`, each written as
  `{{ env.x | default(value='...') }}`. Application settings do NOT go through
  `[vars]` — see the configuration section below.
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
(SMTP + embedded templates), `internal/envflag` (flags fall back to env vars),
`internal/testutil` (fresh migrated test DB per test), `internal/assert` (test
assertion helpers).

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
- Coverage runs **~87% module-wide** (`internal/validator`/`internal/envflag`
  near 100%; the thin spots are mostly `main`/`run` process wiring). No CI
  enforces this number, so treat it as a rough baseline rather than a gate —
  run `mise run test/cover` for current, per-package numbers instead of
  trusting a figure that can only get stale here. Note it uses
  `-coverpkg=./...` — without that flag `internal/db` reads 0%, because it's
  exercised through `internal/testutil` from other packages.
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

## Configuration

Every setting is a flag with an environment variable behind it. Precedence is
**flag > env > default**; the env name is the flag upper-cased with dashes
turned to underscores and a `GREENLIGHT_` prefix (`-smtp-password` →
`GREENLIGHT_SMTP_PASSWORD`). `internal/envflag` implements this in one
`fs.Visit` loop and adds no dependency — don't replace it with viper/kong.

- `go run ./cmd/api -help` is the generated reference for every setting; it
  can't drift, so don't duplicate the list into docs.
- Both `cmd/api` and `cmd/seed` build their **own** `flag.FlagSet` (not
  `flag.CommandLine`, which parses once per process and would make `run()`
  untestable). `cmd/api`'s lives in `parseConfig`.
- `-version` is excluded from the env mapping on purpose: `GREENLIGHT_VERSION`
  is plausible for a deployment to set for other reasons and would fail to
  parse as a bool, stopping boot. `TestParseConfigIgnoresVersionEnv` guards it.
- **Never add a `-db-dsn` flag back to the `run/api` or `run/seed` mise tasks.**
  A flag outranks the environment, so it would silently ignore `.env` while the
  `db/*` tasks still honoured it — you'd be inspecting a different database
  from the one the server has open.
- The `db/*` tasks expand `${GREENLIGHT_DB_DSN:-greenlight.db}` in the shell,
  not via `{{ vars.x }}`. mise renders templates from the process environment
  before `_.file = ".env"` is loaded, so a template silently misses `.env`.
  (Also: `.env` beats the surrounding shell environment in mise.)

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
