# Architecture

A simplified map of how Greenlight is put together. For deep-dive rationale
(every SQLite trade-off, every design decision) see [README.md](README.md) —
this file is the 30,000-foot view.

---

## 1. What this is

A JSON API for a movie database. One Go binary, one SQLite file. No external
services required to run it — no Postgres, no Docker, no message queue.

```
                        ┌─────────────────────┐
   HTTP request  ─────► │   greenlight binary   │ ─────► greenlight.db (SQLite file)
                        └─────────────────────┘
```

---

## 2. The three layers

The whole codebase is three layers, each with one job. A request always
flows top to bottom; nothing skips a layer.

```
┌──────────────────────────────────────────────────────────────┐
│  HTTP layer            cmd/api/                                │
│  "translate HTTP ⇄ Go"                                          │
│  routing, middleware, request/response JSON, auth enforcement   │
└───────────────────────────┬──────────────────────────────────┘
                             │  calls methods on app.models.*
                             ▼
┌──────────────────────────────────────────────────────────────┐
│  Model layer            internal/data/                          │
│  "domain types + SQL"                                           │
│  Movie, User, Token, Permissions — knows nothing about HTTP     │
└───────────────────────────┬──────────────────────────────────┘
                             │  database/sql calls
                             ▼
┌──────────────────────────────────────────────────────────────┐
│  Storage                internal/db/  +  migrations/            │
│  opens the SQLite file, applies schema migrations on boot       │
└──────────────────────────────────────────────────────────────┘
```

Two small supporting packages sit beside these: `internal/validator` (turns
bad input into field-level error messages) and `internal/mailer` (sends the
activation email).

**Why this separation matters:** the model layer never imports `net/http`,
and handlers never write raw SQL. You can unit-test business rules without a
server, and swap out the transport (HTTP today) without touching the data
layer.

---

## 3. Directory map

```
cmd/api/       The HTTP server — everything transport-specific.
  main.go        wires everything together: config, logger, db, models, mailer
  routes.go      the URL → handler table, and the middleware chain
  auth.go, cors.go, logging.go,          one file per middleware concern
  panic_recovery.go, rate_limit.go,
  security_headers.go
  *.go           one file per resource: healthcheck, movies, users, tokens
  helpers.go     shared JSON read/write helpers
  errors.go      every error response shape

cmd/seed/      A CLI that creates a demo user + sample movies (not from the book).

internal/      Go-enforced private code — nothing outside this module can import it.
  data/          domain types (Movie, User, Token...) and their SQL
  db/            opens the SQLite connection pool, runs migrations
  mailer/        SMTP client + embedded email templates
  validator/     generic "collect field errors into a map" helper
  assert/        tiny generic assertion helpers used by every test
  testutil/      spins up a fresh migrated test database per test
                 (doc.go is the guide to the whole test suite)

migrations/    Versioned .sql schema files, embedded into the binary.
```

**Why laid out this way:**

- **`cmd/` vs `internal/` is standard Go project convention, not something
  invented here.** `cmd/` holds `main` packages, one subdirectory per binary
  (`cmd/api` for the server, `cmd/seed` for the demo-data command).
  `internal/` is special to the Go toolchain itself: a package rooted under an
  `internal/` directory can only be imported by code rooted at that
  directory's parent. The compiler enforces this — it isn't a lint rule
  someone can ignore. That gives the module a real private/public boundary,
  which is why `data`, `db`, `mailer`, `validator`, and `testutil` all live
  there: none of them are meant to be imported by anything outside this repo.
- **Inside `internal/`, the split mirrors the three layers from §2.**
  `internal/data` is the model layer and `internal/db` is storage — keeping
  them as separate packages is what makes it physically impossible for
  `internal/data` to accidentally depend on `net/http`, and it's what lets
  `internal/db` be swapped or tested (via `internal/testutil`) without
  touching a single query.
- **`mailer`, `validator`, and `testutil` don't fit the three layers** — they're
  cross-cutting support code — but they still need the same
  compiler-enforced privacy, so they sit beside `data`/`db` under `internal/`
  rather than in their own top-level directory.

---

## 4. Request lifecycle

Every request passes through a fixed middleware chain before it reaches a
handler. The order is deliberate (see comments in `routes.go`):

```
  request
     │
     ▼
  requestID        random ID → X-Request-Id header + request context, so the
     │              client's response and every log line for it can be matched
     ▼
  metrics          count it, start timing it
     │
     ▼
  recoverPanic     turn any panic below into a clean 500 instead of a crash
     │
     ▼
  secureHeaders    X-Frame-Options, CSP, etc.
     │
     ▼
  enableCORS       origin allow-list (runs BEFORE rateLimit, so a rejected
     │              request still gets its CORS headers)
     ▼
  rateLimit        per-IP token bucket → 429 if exhausted
     │
     ▼
  authenticate     Bearer token → *data.User in the request context
     │              (or the "anonymous" user — this step never rejects)
     ▼
  logRequest       log method, URI, status, duration once the handler is done
     │
     ▼
  router (http.ServeMux)   matches the URL, extracts path params like {id}
     │
     ▼
  requirePermission("movies:read")     ─┐
     └─ requireActivatedUser            │  these three compose: asking for a
          └─ requireAuthenticatedUser  ─┘  permission automatically requires
                                            an activated, authenticated user
     │
     ▼
  handler          e.g. showMovieHandler: parse input → call the model →
     │              write JSON response
     ▼
  model            e.g. MovieModel.Get: run SQL, scan rows into Go structs
     │
     ▼
  SQLite
```

Handlers are all methods on `*application` (defined in `main.go`), which
holds the logger, config, models, and mailer. There are no global variables —
that's what lets each test build a completely independent app instance.

---

## 5. Data model

```
┌────────────┐        ┌──────────────────┐        ┌─────────────┐
│   users    │───────►│ users_permissions │◄───────│ permissions │
│            │  1:N   │  (join table)     │  N:1   │             │
│ id         │        │ user_id           │        │ id          │
│ name       │        │ permission_id     │        │ code        │
│ email      │        └──────────────────┘        └─────────────┘
│ password_  │                                     e.g. "movies:read",
│  hash      │                                          "movies:write"
│ activated  │
│ version    │
└─────┬──────┘
      │ 1:N (ON DELETE CASCADE)
      ▼
┌────────────┐
│   tokens   │   hash    (SHA-256 of the plaintext token — plaintext is
│            │            never stored)
│ user_id    │   scope   ("activation" or "authentication" — an emailed
│ expiry     │            activation token can't be replayed as a login)
│ scope      │
└────────────┘

┌────────────┐
│   movies   │   standalone — no foreign keys to users
│            │
│ id         │   genres is a JSON array stored as TEXT (SQLite has no
│ title      │   array type); internal/data/genres.go makes it transparent
│ year       │   at the call sites via the Valuer/Scanner interfaces
│ runtime    │
│ genres     │   version powers optimistic locking: every UPDATE checks
│ version    │   `WHERE id = ? AND version = ?`, so a stale write affects
└────────────┘   zero rows and comes back as 409 Conflict instead of
                  silently overwriting someone else's change.
```

---

## 6. Auth & permissions, end to end

1. `POST /v1/users` — register. A row is created in `users` (unactivated),
   and an activation token (scope `activation`) is emailed — or logged, if no
   SMTP host is configured.
2. `PUT /v1/users/activated` — the client sends the token back; it's hashed
   and matched against `tokens`, the user is flipped to `activated`, and the
   token is deleted (single-use).
3. `POST /v1/tokens/authentication` — email + password in, a new token
   (scope `authentication`) out. This is the bearer token used on every
   subsequent request.
4. Every request — `Authorization: Bearer <token>` is hashed and looked up
   in `tokens` by the `authenticate` middleware, which resolves it to a
   `*data.User` and stashes it on the request context.
5. Route-level guards (`requirePermission`, `requireActivatedUser`,
   `requireAuthenticatedUser`) read that user back out of the context and
   check `activated` / the `users_permissions` join table before letting the
   handler run.

---

## 7. Process lifecycle: startup, serving, shutdown

Three phases, spread across two files: `cmd/api/main.go` (`main` → `run`) and
`cmd/api/server.go` (`serve`).

### 7.1 Startup — `main()` → `run()`

```
main()
  │   does as little as possible: call run(), print its error, exit(1).
  ▼
run()                    returns an error instead of calling os.Exit —
  │                      os.Exit skips deferred calls, and this function
  │                      has one that matters (see below)
  │
  ├─ flag.Parse()             → config struct: port, env, db dsn + pool,
  │                             limiter, smtp, cors. Every knob, one place.
  ├─ slog JSON handler        → structured logs on stdout
  ├─ db.OpenDB()              → SQLite pool; PRAGMAs ride in the DSN
  │    └─ defer database.Close()    ← the whole reason run() ≠ main()
  ├─ db.MigrateUp()           → embedded migrations/*.sql; no-op if current
  ├─ expvar.Publish(…)        → version, goroutine count, pool stats,
  │                             served lazily at GET /debug/vars
  ├─ &application{…}          → config, logger, models, mailer,
  │                             wg (WaitGroup), shutdown (chan struct{})
  └─ app.serve()              → blocks for the rest of the process's life
```

Nothing here can silently fail: every step returns an `error` up to `main()`,
and because `run()` returns rather than exits, `database.Close()` always runs.

### 7.2 Serving — steady state

`serve()` builds the `http.Server` (handler, timeouts on every axis, the app's
`slog` handler for the server's *own* errors), then four kinds of goroutine
coexist:

```
┌─────────────────────┬──────────────────────────────────────────────────┐
│ main goroutine      │ blocked in srv.ListenAndServe()                  │
├─────────────────────┼──────────────────────────────────────────────────┤
│ signal goroutine    │ blocked on <-quit, waiting for SIGINT/SIGTERM    │
├─────────────────────┼──────────────────────────────────────────────────┤
│ janitor goroutine   │ started by rateLimit when app.routes() was       │
│                     │ called; sweeps stale client IPs every minute;    │
│                     │ selects on app.shutdown  → "tell it to STOP"     │
├─────────────────────┼──────────────────────────────────────────────────┤
│ 0..n background     │ spawned per request by app.background() —        │
│ goroutines          │ welcome/activation email, each with its own      │
│                     │ recover; tracked by app.wg  → "wait to FINISH"   │
└─────────────────────┴──────────────────────────────────────────────────┘
```

Note that `app.routes()` is not purely a wiring call — building the chain is
what starts the janitor.

### 7.3 Shutdown — the ordering is the design

```
   SIGINT (Ctrl+C) / SIGTERM (Docker, k8s, systemd)
        │                       SIGKILL is deliberately absent: uncatchable
        ▼
 ┌─ signal goroutine ──────────────────┐   ┌─ main goroutine ──────────────┐
 │                                     │   │                               │
 │ srv.Shutdown(ctx)  ─── unblocks ────┼──►│ ListenAndServe returns        │
 │   1. stop accepting connections     │   │ http.ErrServerClosed          │
 │   2. close idle keep-alives         │   │   ↑ the SUCCESS case, and it  │
 │   3. wait for active ones to finish │   │     arrives BEFORE the drain  │
 │   ctx deadline: 30s                 │   │     has finished              │
 │        └─ blown? send err, skip     │   │                               │
 │           straight to the send      │   ▼                               │
 │        │ drained cleanly            │   <-shutdownError   (blocks here) │
 │        ▼                            │   │  this receive is what makes   │
 │ app.stop()                          │   │  the shutdown graceful        │
 │   close(app.shutdown)               │   │                               │
 │   → janitor's select returns        │   │                               │
 │        │                            │   │                               │
 │        ▼                            │   │                               │
 │ app.wg.Wait()                       │   │                               │
 │   in-flight emails land             │   │                               │
 │   (no deadline of its own)          │   │                               │
 │        │                            │   │                               │
 │        ▼                            │   │                               │
 │ shutdownError <- nil  ──────────────┼──►│ …the receive completes        │
 └─────────────────────────────────────┘   │              │                │
                                           │              ▼                │
                                           │ serve() returns nil           │
                                           └──────────────┬────────────────┘
                                                       ▼
                                        run()'s defer: database.Close()
                                                       │
                                                       ▼
                                                   process exits
```

Each step is where it is for a reason:

- **`srv.Shutdown` first.** Drain requests before dismantling anything they
  might still depend on.
- **`app.stop()` second**, once nothing is being served — killing the janitor
  while requests were live would pull the rug from under an active handler.
- **`app.wg.Wait()` last**, because that work has no other exit condition.
- **The main goroutine blocks on `shutdownError`.** Without that receive,
  `serve()` would return the instant `Shutdown` was *called*, `run()` would
  close the database, and the process would exit with requests and emails still
  in flight. `TestServeGracefulShutdown` asserts precisely this.

Three consequences worth knowing (rationale in
[README.md](README.md#startup-serving-and-shutdown)):

- The 30s grace bounds `srv.Shutdown` **only** — `app.wg.Wait()` is unbounded.
- A **second Ctrl+C does nothing**; `signal.Notify` replaced the default
  terminate behaviour. Aborting a wedged drain takes `SIGKILL`.
- Any error from `ListenAndServe` other than `ErrServerClosed` (port in use,
  say) returns immediately, leaving the signal goroutine parked forever —
  harmless on the way out, but it makes `serve()` a once-per-process call.

---

## 8. A few load-bearing decisions

- **Dependency injection via a struct, not globals.** Every handler is a
  method on `*application`. Lets tests build a fully isolated app per test.
- **The model layer only ever returns sentinel errors** (`ErrRecordNotFound`,
  `ErrEditConflict`) upward — handlers translate those into HTTP status
  codes. SQL details never leak past `internal/data`.
- **Every test gets its own SQLite file** in `t.TempDir()`, migrated fresh.
  No mocking the database, no shared fixtures, safe to run in parallel.
- **Background work (sending email) gets its own panic recovery.** The
  `recoverPanic` middleware only protects the request's own goroutine — a
  panic in a spawned goroutine kills the whole process otherwise, so
  `app.background()` wraps every background task in its own `recover`.
- **Two mechanisms for background goroutines, solving opposite problems.**
  `app.wg` means *wait for this work to finish* — it tracks per-request tasks
  like sending a welcome email, and graceful shutdown blocks on it.
  `app.shutdown` means *tell this loop to stop* — it's a channel closed by
  `app.stop()`, which long-lived loops (currently the rate limiter's client-map
  janitor) select on. Waiting on an endless loop would hang forever; telling a
  half-sent email to stop would lose it.

---

## 9. How it's tested

One external test dependency (`github.com/google/go-cmp`); everything else is
the standard library. The full guide — commands, conventions, and the Go
testing machinery involved — lives in
[`internal/testutil/doc.go`](internal/testutil/doc.go), readable with
`go doc greenlight/internal/testutil`.

```
mise run test        go test -race ./...        the everyday command
mise run test/short  skips database-backed tests
mise run test/cover  coverage (-coverpkg=./...), opens an HTML report
mise run test/fuzz   30s of fuzzing per package
mise run test/bench  benchmarks with allocation counts
mise run audit       tidy + fmt + vet + test -race
mise run lint         golangci-lint (CLI auto-installed via mise.toml [tools])
mise run vuln         govulncheck (CLI auto-installed via mise.toml [tools])
```

**Layers of the suite, roughly outside-in:**

| Layer | Where | What it proves |
|---|---|---|
| End-to-end HTTP | `cmd/api/*_test.go` | Real `httptest.Server`, real DB, real middleware chain. Only outbound email is mocked. |
| Graceful shutdown | `cmd/api/server_test.go` | `serve()` under a real SIGTERM: in-flight requests and background email both complete before it returns. |
| Model layer | `internal/data/*_test.go` | The SQL itself, against a real schema. |
| Concurrency | `internal/data/concurrency_test.go` | `busy_timeout` under contention, WAL readers during writes, and that exactly one of two racing updates wins. |
| Connection setup | `internal/db/db_test.go` | Every PRAGMA reaches *every* pooled connection, and migrations are idempotent across restarts. |
| Email | `internal/mailer/mailer_test.go` | Template rendering and MIME structure, asserted on the bytes sent to a fake in-process SMTP server. |
| Pure units | `internal/validator`, parts of `internal/data` | No I/O; microseconds. |
| Fuzzing | 4 targets | The hand-written parsers: `Runtime.UnmarshalJSON`, `Genres.Scan`, `readJSON`, and the email regexp. |
| Benchmarks | `*/bench_test.go` | Baselines for bcrypt cost, `GetAll` with filters, JSON encoding, and the middleware chain. |

**Two things worth knowing:**

- **No database mocks, anywhere.** SQLite is an embedded library, so a "real
  database" is a temp file that costs about a millisecond. Since the thing most
  likely to be wrong in a model layer is the SQL, and a mock cannot check SQL,
  this is a far better trade than it would be against Postgres.
- **`Mailer` is an interface for exactly one reason.** The book stores a
  concrete `*mailer.Mailer` on the application; this uses a one-method interface
  so tests can inject a recorder and read the activation token out of it. That
  single deviation is what makes the whole registration → activation flow
  testable. The real implementation is then covered separately in
  `internal/mailer`.

Coverage after the suite as it stands: `internal/validator` 100%,
`internal/mailer` 93%, `internal/data` 91%, `internal/db` 84%, `cmd/api` 81%,
`cmd/seed` 73% — 84% across the module. The bulk of what's left is `main()`,
`run()`, and other process wiring.

---

For the full reasoning behind each of these — and the complete list of
places SQLite forced a change from the book Postgres schema — see
[README.md](README.md).
