# Greenlight — a JSON API in Go, on SQLite

A complete, heavily-commented JSON API for a movie database, built with the
techniques from Alex Edwards' two books — **[Let's Go][letsgo]** and
**[Let's Go Further][further]** — but backed by **SQLite** instead of
PostgreSQL.

It's written as a learning project. Nearly every non-obvious line has a comment
explaining *why* it's there, not just what it does, and this README explains the
architecture and every place where SQLite forced a change from the book.

[letsgo]: https://lets-go.alexedwards.net/
[further]: https://lets-go-further.alexedwards.net/

---

## Table of contents

1. [Quickstart](#quickstart)
2. [Trying it in Bruno](#trying-it-in-bruno)
3. [API endpoints](#api-endpoints)
4. [Project layout](#project-layout)
5. [How a request flows through the app](#how-a-request-flows-through-the-app)
6. [PostgreSQL → SQLite: every difference](#postgresql--sqlite-every-difference)
7. [The packages, and why each one](#the-packages-and-why-each-one)
8. [Design decisions worth understanding](#design-decisions-worth-understanding)
9. [Testing](#testing)
10. [Where this deviates from the books](#where-this-deviates-from-the-books)
11. [Going further from here](#going-further-from-here)

---

## Quickstart

You need **Go 1.22+** and nothing else. No database server, no Docker, no C
compiler.

```bash
# 1. Seed a demo user (activated, with full permissions) and sample movies.
#    This creates and migrates greenlight.db on the spot.
make run/seed

# 2. Start the API.
make run/api
```

`make run/seed` prints an authentication token — it's fixed (`GREENLIGHT0000000000000000` by default, override with `-token`), so it's the same every time rather than something you copy out of the output each run. In another terminal:

```bash
export TOKEN=GREENLIGHT0000000000000000

# List movies
curl -H "Authorization: Bearer $TOKEN" localhost:4000/v1/movies

# Filter, sort and paginate
curl -H "Authorization: Bearer $TOKEN" \
  'localhost:4000/v1/movies?genres=action,adventure&sort=-year&page_size=2'

# Create one
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:4000/v1/movies \
  -d '{"title":"Dune","year":2021,"runtime":"155 mins","genres":["sci-fi"]}'

# Public endpoint, no token needed
curl localhost:4000/v1/healthcheck
```

To try the real signup flow instead of the seeded user:

```bash
curl -X POST localhost:4000/v1/users \
  -d '{"name":"Alice","email":"alice@example.com","password":"pa55word1234"}'
```

No SMTP server is configured by default, so the welcome email is **logged
instead of sent** — the activation token appears right in the server's log
output. Copy it and activate:

```bash
curl -X PUT localhost:4000/v1/users/activated -d '{"token":"<token from the log>"}'

curl -X POST localhost:4000/v1/tokens/authentication \
  -d '{"email":"alice@example.com","password":"pa55word1234"}'
```

Run `make help` to see every available target.

---

## Trying it in Bruno

A [Bruno](https://www.usebruno.com/) collection lives in `bruno/`, with one
request per endpoint in the table below. Open the `bruno/` folder in the
Bruno VS Code extension (recommended in `.vscode/extensions.json`) or the
desktop app, then select the `Local` environment — it holds `baseUrl` and
`token`.

**Fast path**, using the seeded demo user (already has both `movies:read` and
`movies:write`):

1. `make run/seed` — the demo user's token is fixed
   (`GREENLIGHT0000000000000000`), so `Local`'s `token` already matches it.
   No copying required, even across reseeds.
2. `make run/api`
3. Send any request in the collection.

**Real signup flow**, exercising registration/activation/login instead of the
seeded user:

1. Send **Users → Register User** (202). The activation email is logged to
   the `make run/api` terminal instead of sent — copy the token from there.
2. Paste it into **Users → Activate User**'s body and send (200).
3. Send **Tokens → Authenticate** with the same credentials (201), and copy
   `authentication_token.token` from the response into `Local`'s `token`.

Two things that look like bugs but aren't:

- **A self-registered user only gets `movies:read`** (see
  `registerUserHandler` in `cmd/api/users.go`) — POST/PATCH/DELETE on movies
  will 403 for that user. Only the seeded demo user can write.
- **The rate limiter is 2 req/s with a burst of 4** — running the whole
  collection back-to-back will 429 partway through. That's the limiter
  working as intended, not a broken request.

---

## API endpoints

| Method   | Pattern                       | Permission      | Description                          |
| -------- | ----------------------------- | --------------- | ------------------------------------ |
| `GET`    | `/v1/healthcheck`             | *public*        | Liveness and build info              |
| `GET`    | `/v1/movies`                  | `movies:read`   | List movies (filter/sort/paginate)   |
| `POST`   | `/v1/movies`                  | `movies:write`  | Create a movie                       |
| `GET`    | `/v1/movies/{id}`             | `movies:read`   | Show one movie                       |
| `PATCH`  | `/v1/movies/{id}`             | `movies:write`  | Partially update a movie             |
| `DELETE` | `/v1/movies/{id}`             | `movies:write`  | Delete a movie                       |
| `POST`   | `/v1/users`                   | *public*        | Register (sends activation email)    |
| `PUT`    | `/v1/users/activated`         | *public*        | Activate with an emailed token       |
| `POST`   | `/v1/tokens/authentication`   | *public*        | Exchange credentials for a token     |
| `GET`    | `/debug/vars`                 | *public* ⚠️      | expvar runtime metrics               |

⚠️ `/debug/vars` exposes memory statistics, the process command line and
database pool internals. In production, put it behind authentication or bind it
to an internal-only interface. It's open here because this is a learning
project.

Every response — success *and* error — is JSON wrapped in a single top-level
key:

```jsonc
// 200 OK
{ "movie": { "id": 1, "title": "Casablanca", "runtime": "102 mins", ... } }

// 422 Unprocessable Entity
{ "error": { "title": "must be provided", "year": "must be greater than 1888" } }
```

---

## Project layout

```
.
├── cmd/
│   ├── api/              The API server. Everything HTTP-specific lives here.
│   │   ├── main.go       Config, flags, dependency wiring, expvar setup
│   │   ├── server.go     HTTP server + graceful shutdown
│   │   ├── routes.go     Router and the middleware chain
│   │   ├── middleware.go 8 middlewares: panic recovery, rate limiting, auth…
│   │   ├── context.go    Type-safe storage of the current user in the context
│   │   ├── helpers.go    JSON read/write, query-string parsing
│   │   ├── errors.go     Every error response the API can produce
│   │   ├── healthcheck.go / movies.go / users.go / tokens.go   Handlers
│   │   └── *_test.go     End-to-end tests over a real HTTP server
│   └── seed/             Demo-data command (not from the books)
│
├── internal/             Private to this module — the Go toolchain *enforces*
│   │                     that nothing outside can import it.
│   ├── data/             The model layer: domain types + all the SQL
│   ├── db/               Opening SQLite, and running migrations
│   ├── mailer/           SMTP + embedded HTML/text email templates
│   ├── validator/        Collects validation errors into a map
│   ├── assert/           Tiny test-assertion helpers (from book 1)
│   └── testutil/         Builds a fresh migrated database per test
│
├── migrations/           Versioned .sql schema, embedded into the binary
├── Makefile
└── README.md
```

The `internal/` convention is worth internalising: it isn't a naming style, it's
a rule the compiler enforces. Any package under an `internal/` directory can
only be imported by code rooted at that directory's parent. It gives you a real
private/public boundary within a module.

---

## How a request flows through the app

Take `GET /v1/movies/1` with a valid bearer token:

```
   HTTP request
        │
        ▼
┌───────────────────┐
│ metrics           │  count the request; start the timer
├───────────────────┤
│ recoverPanic      │  turn any panic below into a clean JSON 500
├───────────────────┤
│ secureHeaders     │  X-Frame-Options, CSP, …
├───────────────────┤
│ enableCORS        │  Access-Control-Allow-Origin for trusted origins
├───────────────────┤
│ rateLimit         │  per-IP token bucket → 429 if exhausted
├───────────────────┤
│ authenticate      │  Bearer token → *data.User (or AnonymousUser) in context
├───────────────────┤
│ logRequest        │  log method, URI, status, duration
└─────────┬─────────┘
          ▼
    http.ServeMux       match the route, extract {id}
          │
          ▼
  requirePermission("movies:read")
          └─ requireActivatedUser
                └─ requireAuthenticatedUser   ← composed, so each implies the last
          │
          ▼
   showMovieHandler     readIDParam → models.Movies.Get(id) → writeJSON
          │
          ▼
   MovieModel.Get       SQL, then Genres.Scan decodes the JSON column
```

**The middleware order is deliberate** and documented in `routes.go`. Two
orderings that matter:

- `recoverPanic` sits near the outside so it catches panics from everything
  below it.
- `enableCORS` runs *before* `rateLimit`, so that a rejected request still gets
  its CORS headers. Otherwise a browser reports a confusing CORS error instead
  of the real 429.

**The permission wrappers compose.** `requirePermission` wraps
`requireActivatedUser`, which wraps `requireAuthenticatedUser`. You cannot
accidentally let an anonymous user through by forgetting a wrapper, because
asking for a permission automatically demands everything beneath it.

---

## PostgreSQL → SQLite: every difference

This is the interesting part of the project. The book's application logic ports
over essentially unchanged; the changes are all at the SQL boundary.

### 1. Placeholders: `$1` → `?`

Postgres uses numbered placeholders, SQLite uses positional ones. Mechanical,
but it's every query.

```sql
-- Book                                    -- Here
WHERE id = $1 AND version = $2             WHERE id = ? AND version = ?
```

### 2. Arrays → JSON text

The book stores genres in a native `text[]` column and moves them with
`pq.Array`. SQLite has **no array type** — only NULL, INTEGER, REAL, TEXT and
BLOB.

We store the genres as a JSON string and implement the two standard
`database/sql` interfaces so the encoding is invisible at the call sites
(`internal/data/genres.go`):

```go
func (g Genres) Value() (driver.Value, error)  // Go → database
func (g *Genres) Scan(src any) error           // database → Go
```

Implement those and `Genres` can be passed to `Exec` and scanned from `Scan`
exactly like a `string`. All the awkwardness is confined to one file.

Genre **containment** filtering (`genres @> $2` in the book) uses SQLite's
built-in JSON1 extension. `json_each()` explodes a JSON array into rows, so
"the movie has all of the requested genres" becomes:

```sql
AND (
    json_array_length(?) = 0                    -- no filter supplied
    OR (
        SELECT count(*)
        FROM json_each(?) AS requested
        WHERE EXISTS (
            SELECT 1 FROM json_each(movies.genres) AS mg
            WHERE mg.value = requested.value
        )
    ) = json_array_length(?)                    -- all of them matched
)
```

Nicely, this keeps a **fixed** number of placeholders no matter how many genres
the client asks for, so there's no dynamic SQL to build.

### 3. `citext` → `COLLATE NOCASE`

The book installs the `citext` extension so email lookups are case-insensitive.
SQLite can't install extensions, but it has collations, which do the same job
for both the `UNIQUE` index and `WHERE email = ?`:

```sql
email TEXT NOT NULL UNIQUE COLLATE NOCASE
```

*Caveat:* `NOCASE` only folds ASCII A–Z, not full Unicode. Fine for email
addresses in practice.

### 4. Full-text search → `LIKE`

The book builds a GIN index over `to_tsvector('simple', title)` for real
full-text search. SQLite's equivalent is the FTS5 extension, which needs a
separate virtual table kept in sync by triggers.

To keep the moving parts down, this project uses a plain `LIKE`:

```sql
WHERE (? = '' OR title LIKE '%' || ? || '%')
```

SQLite's `LIKE` is **case-insensitive for ASCII by default**, so this gets the
case-insensitivity for free. The trade-off is honest: a leading-wildcard `LIKE`
is a table scan and can't use an index. That's completely fine for thousands of
movies, and not fine for millions — see [Going further](#going-further-from-here)
for the FTS5 upgrade.

### 5. Error inspection: string matching → typed errors

The book detects a duplicate email by comparing the driver's error *message*:

```go
case err.Error() == `pq: duplicate key value violates unique constraint "users_email_key"`:
```

That breaks if the constraint is renamed or the driver reformats its messages.
`modernc.org/sqlite` gives us a typed error with SQLite's numeric result code,
which is far more robust:

```go
var sErr *sqlite.Error
if errors.As(err, &sErr) {
    return sErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE      // 2067
        || sErr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY  // 1555
}
```

### 6. Foreign keys are OFF by default ⚠️

This is the SQLite footgun most likely to bite you. Unlike every other SQL
database, **SQLite does not enforce foreign keys unless you ask it to**, per
connection. Without the pragma, every `REFERENCES` clause and every
`ON DELETE CASCADE` in the schema is silently decorative.

We enable it in the DSN, and there are dedicated tests
(`TestTokenModel_CascadeDeleteOnUser`) that would fail if it were ever removed.

### 7. Connection PRAGMAs belong in the DSN

PRAGMAs are **per-connection** settings, and `database/sql` manages a *pool*
that opens and closes connections as it sees fit. Running
`PRAGMA foreign_keys = ON` once after opening would only configure whichever
connection happened to serve that one statement.

Putting them in the DSN makes the driver apply them to *every* connection
(`internal/db/db.go`):

```
file:greenlight.db?_pragma=foreign_keys(1)
                  &_pragma=journal_mode(WAL)
                  &_pragma=busy_timeout(5000)
                  &_pragma=synchronous(NORMAL)
```

| PRAGMA | Why |
| ------ | --- |
| `foreign_keys(1)` | See above — without it, FKs do nothing. |
| `journal_mode(WAL)` | Write-Ahead Logging. In the default rollback-journal mode a writer blocks all readers. Under WAL, readers and one writer proceed concurrently — essential for a web server. |
| `busy_timeout(5000)` | SQLite allows only **one writer at a time**. Without this, a concurrent write fails instantly with `SQLITE_BUSY`; with it, the driver waits up to 5s for the lock. Turns a hard error into a latency blip. |
| `synchronous(NORMAL)` | The recommended pairing with WAL: durable against application crashes, only at risk from OS/power failure. `FULL` fsyncs every commit and is dramatically slower. |

### 8. Connection pool sizing

The book sets `MaxOpenConns` to 25 because Postgres genuinely handles 25
concurrent writers. SQLite serialises **all** writes behind a single
database-level lock, so a large pool buys nothing for writes — it only helps
concurrent reads (which WAL does allow in parallel).

We default to 4. The rigorous alternative is *two* pools — a read pool with N
connections and a write pool pinned to exactly 1, which eliminates
`SQLITE_BUSY` entirely. It's a great pattern, but it means threading two handles
through every model method, so this project sticks with one pool plus
`busy_timeout`.

### 9. Schema type mapping

| Postgres | SQLite | Note |
| -------- | ------ | ---- |
| `bigserial PRIMARY KEY` | `INTEGER PRIMARY KEY AUTOINCREMENT` | `AUTOINCREMENT` prevents id *reuse* after a delete — important for a public API, or a new record could inherit a deleted one's URL. |
| `timestamp(0) with time zone` | `TIMESTAMP` | The driver reads the *declared* type and converts to/from `time.Time` automatically. |
| `text[]` | `TEXT` (JSON) | See above. |
| `citext` | `TEXT COLLATE NOCASE` | See above. |
| `bytea` | `BLOB` | For bcrypt hashes and token hashes. |
| `bool` | `BOOLEAN` | Stored as 0/1; the declared type gets you a real Go `bool`. |

### 10. `ALTER TABLE` can't add constraints

The book adds `CHECK` constraints in a second migration. SQLite's `ALTER TABLE`
can't do that, so they're declared inline in the `CREATE TABLE` instead.

### What *didn't* change

Pleasantly, a lot:

- **`RETURNING`** works (SQLite 3.35+), so inserts and updates still fetch
  generated columns in one round trip.
- **`count(*) OVER()`** works, so pagination metadata still comes from the same
  query as the results rather than a second `COUNT`.
- **Optimistic locking** with a `version` column is pure SQL and ports verbatim.
- **Transactions**, `INNER JOIN`, and every other bit of ordinary SQL.

---

## The packages, and why each one

Chosen from across both books:

| Package | Book | Used for |
| ------- | ---- | -------- |
| `justinas/alice` | Let's Go | Turns nested middleware wrapping into a readable list. |
| `felixge/httpsnoop` | both | Captures the status code and bytes written for logging and metrics. Writing your own `ResponseWriter` wrapper silently breaks `Flusher`/`Hijacker`; this doesn't. |
| `tomasen/realip` | Further | Extracts the client IP from `X-Forwarded-For` for per-client rate limiting. |
| `golang.org/x/time/rate` | Further | The token-bucket rate limiter. |
| `golang.org/x/crypto/bcrypt` | Let's Go | Password hashing. |
| `go-mail/mail/v2` | Further | SMTP with multipart text+HTML messages. |
| `golang-migrate/migrate/v4` | Further | Versioned schema migrations. |
| `modernc.org/sqlite` | — | The database driver. |
| `log/slog` | — | Structured JSON logging (stdlib). |
| `net/http.ServeMux` | — | Routing with method-specific patterns and `{id}` wildcards (stdlib). |

### Why `modernc.org/sqlite` and not `mattn/go-sqlite3`?

`mattn/go-sqlite3` is a cgo wrapper around the real C library — fast and
battle-tested, but it needs a C toolchain, breaks `CGO_ENABLED=0` static builds,
and makes cross-compiling painful.

`modernc.org/sqlite` is SQLite **transpiled to Go**. Pure Go means no C
compiler, trivial cross-compilation (`make build/linux` is one command with no
setup), and `go test` that just works on any machine. For a learning project
that's a decisive advantage, and it's plenty fast.

### Why `log/slog` instead of the book's `jsonlog`?

The book predates it and hand-rolls a `jsonlog` package. `log/slog` landed in
Go 1.21, does the same job, and is now the idiomatic choice.

### Why `net/http.ServeMux` instead of `julienschmidt/httprouter`?

The book (and this project, originally) uses `httprouter` for method-specific
routing and named path parameters. Go 1.22 added both directly to
`http.ServeMux` — patterns like `"GET /v1/movies/{id}"` and
`r.PathValue("id")` — so the dependency is no longer pulling its weight.

The one thing `ServeMux` doesn't give you is a hook for custom 404/405 bodies.
`routes.go` gets JSON versions of both without one: every path also gets a
method-less fallback pattern (e.g. `"/v1/movies"` alongside `"GET
/v1/movies"`), and per ServeMux's precedence rules a method-specific pattern
is strictly more specific than the method-less one for the same path, so the
fallback only ever fires when no registered method matches — exactly the 405
case. A catch-all `"/"` pattern handles genuinely unmatched paths — the 404
case.

---

## Design decisions worth understanding

These are the ideas that transfer to any Go API you write.

### Dependency injection through a struct

There is not one global variable in this codebase. Every handler and middleware
is a **method on `*application`**, which holds the config, logger, models and
mailer. That's what makes it possible to construct a completely independent
application per test.

### Custom JSON types

`Runtime` is `int32` underneath but marshals as `"102 mins"`, via `MarshalJSON`
and `UnmarshalJSON` (`internal/data/runtime.go`).

Note the receivers, because it's a classic Go trap:

- `MarshalJSON` uses a **value** receiver, so it applies to both `Runtime` and
  `*Runtime`. With a pointer receiver, encoding a non-addressable value would
  silently fall back to plain integer encoding.
- `UnmarshalJSON` uses a **pointer** receiver — it has to, since it modifies
  the receiver.

### Input structs prevent mass assignment

Handlers never decode a request body straight into a `data.Movie`. They decode
into a local anonymous struct listing exactly the fields a client may supply:

```go
var input struct {
    Title   string       `json:"title"`
    Year    int32        `json:"year"`
    Runtime data.Runtime `json:"runtime"`
    Genres  []string     `json:"genres"`
}
```

Without this, a client could `POST` `{"id": 999, "version": 42}` and set fields
that are supposed to be server-controlled.

### Pointers make PATCH work

`PATCH` is a *partial* update, so we must distinguish "the client sent `year: 0`"
from "the client didn't mention year". With a plain `int32` both look identical —
the zero value. A `*int32` makes `nil` mean "absent":

```go
if input.Year != nil {
    movie.Year = *input.Year
}
```

### Optimistic concurrency control

Two clients both `GET` movie 7 (version 3), both edit, both `PATCH`. Without
protection the second silently clobbers the first — a *lost update*.

The fix costs one column:

```sql
UPDATE movies SET ..., version = version + 1
WHERE id = ? AND version = ?      -- the version we read
RETURNING version
```

The first write matches and moves the record to version 4. The second write's
`WHERE` now matches nothing, so it affects zero rows — we detect that and return
`409 Conflict`. It's "optimistic" because no locks are taken; we just detect
conflicts when they happen.

### Sort safelisting is a security control

You **cannot** parameterise an `ORDER BY` column — SQL doesn't allow
placeholders for identifiers. So the sort column is interpolated into the query
string, which would be a SQL injection hole if unchecked.

`Filters.sortColumn()` checks the value against a safelist the *handler* sets
(never the client) and **panics** otherwise. The panic looks aggressive, but
validation should already have rejected anything unsafe, so reaching it means a
bug that would otherwise be an injection. `TestFilters_sortColumnPanicsOnUnsafeValue`
and `TestListMoviesSQLInjection` both guard this.

### Tokens are hashed, and scoped

Only the **SHA-256 hash** of a token is stored. A database leak yields nothing
an attacker can authenticate with.

Why SHA-256 rather than bcrypt, when passwords get bcrypt? Because tokens are
128 bits from a CSPRNG — there's no dictionary to attack, so bcrypt's deliberate
slowness would only add latency to every authenticated request.

Tokens also carry a **scope**. An activation token emailed in the clear cannot
be replayed as a login credential.

### Background goroutines need their own panic recovery

The `recoverPanic` middleware only covers the request's own goroutine. **A panic
in any other goroutine kills the entire process.** So `app.background()` wraps
every background task with its own `recover`, plus a `sync.WaitGroup` so
graceful shutdown waits for in-flight work.

### Two mechanisms for background goroutines

There are two kinds of goroutine here, and they need opposite treatment:

| | Mechanism | Meaning |
|---|---|---|
| Per-request work (welcome email) | `app.wg` | *Wait for this to **finish**.* |
| Long-lived loops (rate-limiter janitor) | `app.shutdown` + `app.stop()` | *Tell this to **stop**.* |

Mixing them up doesn't work in either direction. `wg.Wait()` on a loop that never
returns hangs forever; signalling a half-sent email to stop just loses the email.
So `serve()` does both, in order: `srv.Shutdown()` drains in-flight requests,
`app.stop()` closes the channel the endless loops select on, then `app.wg.Wait()`
waits for the email to land.

The book writes the janitor as a bare `for { time.Sleep(time.Minute); ... }` with
no way out — nearly harmless in production, since `routes()` is called once. It's
still worth closing: it's a goroutine that outlives the graceful shutdown the
rest of `serve()` works hard to get right, and in a test binary `routes()` runs
once *per test*, so the goroutines pile up and defeat any leak detection.

`app.shutdown` is nil unless someone sets it, and **a receive on a nil channel
blocks forever** — so an `&application{}` built by hand behaves exactly as
before, and only `run()` and the test harness opt in. That Go detail is what
kept this from becoming a change to every construction site.

### Errors don't leak internals

A 500 tells the client only *"the server encountered a problem"*. The actual
error — SQL text, file paths, driver messages — goes to the log. Leaking it to
clients is an information-disclosure vulnerability.

Similarly, a wrong password and an unregistered email return **byte-identical**
responses, so an attacker can't enumerate which addresses have accounts.
`TestCreateAuthenticationToken` asserts this directly.

---

## Testing

```bash
make test          # everything, with the race detector
make test/short    # only the fast unit tests
make test/cover    # open an HTML coverage report
make test/fuzz     # 30s of fuzzing per package
make test/bench    # benchmarks, with allocation counts
make audit         # fmt + vet + tidy + test -race
```

Current coverage: **`internal/validator` 100%, `internal/mailer` 93%,
`internal/data` 91%, `internal/db` 84%, `cmd/api` 81%, `cmd/seed` 73%** —
**84% across the module**. Most of what's left is `main`/`run`, which is process
wiring.

> **A note on measuring it.** `make test/cover` passes `-coverpkg=./...`. Without
> it, `go test -cover` only instruments the package whose tests are running, so
> `internal/db` reported **0%** despite being exercised by nearly every test in
> the repo through `internal/testutil`. If a package looks suspiciously
> uncovered, check whether it's only ever reached from other packages before
> concluding it's untested.

**One external test dependency: `github.com/google/go-cmp`.** Everything else is
the standard library. It's there because `assert.Equal` is constrained to
`comparable`, which rules out the types this codebase compares most —
`data.Genres` (a slice), `[]*data.Movie`, and the validator's
`map[string]string`. `assert.DeepEqual` wraps `cmp.Diff` and prints a readable
diff instead of two dumped structs. Notably **not** added: testify (the project
deliberately hand-rolls three assertion helpers instead — see
`internal/assert/assert.go`), an HTTP assertion DSL (the `testServer` harness is
100 readable lines), or any database mock.

The full guide to the suite — every command, the conventions, and the Go testing
machinery it uses — is in
[`internal/testutil/doc.go`](internal/testutil/doc.go):

```bash
go doc greenlight/internal/testutil
```

### Two layers of tests

**Unit tests** — no database, no network. Pure logic: the validator, the
`Runtime` JSON codec, the `Genres` Valuer/Scanner, pagination arithmetic. These
run in milliseconds.

**Integration tests** — a real SQLite database with the real schema, and for
`cmd/api`, a real HTTP server via `httptest`. Nothing is mocked except outbound
email.

Every test gets **its own database**, created in `t.TempDir()` and migrated from
the embedded `.sql` files (`internal/testutil/testdb.go`). That means complete
isolation, safe parallelism, no fixtures, and no truncating tables between
cases. Go deletes the directory automatically.

> This is a genuine advantage of SQLite for a learning project. With Postgres
> you'd need Docker or a live server to test the model layer, which is why the
> book's models are much less tested than its handlers. Here there's no excuse
> not to test the SQL — so we do, thoroughly.

`go test -short` skips everything database-backed, following the convention from
book 1.

### What the tests actually check

Beyond the happy paths, the suite is deliberately weighted toward the things
that are easy to break and expensive to get wrong:

- **The full access-control matrix** — anonymous, invalid token, unactivated,
  activated-without-permission, read-only, read-write — against every endpoint
  (`TestAuthenticationAndPermissions`). Authorization bugs are one wrong wrapper
  in `routes.go` away.
- **SQL injection** through the sort parameter.
- **Optimistic locking** actually preventing a lost update.
- **Foreign key cascades** firing, which proves the `foreign_keys` pragma is
  live.
- **Activation tokens being single-use.**
- **Password hashes never appearing in a response.**
- **Login failures being indistinguishable** from each other.
- **Panic recovery** returning a clean 500 without leaking the panic value.
- **Every PRAGMA reaching every pooled connection** — `internal/db` checks out
  four connections *simultaneously* and asserts on all of them, which is what
  defeats the naive "run `PRAGMA foreign_keys = ON` once at startup" mistake
  that a single-connection test would happily pass.
- **Graceful shutdown**, driven by a real `SIGTERM`: in-flight requests finish
  and the background welcome email completes before `serve()` returns.
- **The rate limiter being per-client**, so one noisy address can't lock
  everyone out.
- **Concurrent writers**, which is where `busy_timeout` and WAL earn their keep.

### Three kinds of test beyond the usual

**Concurrency** (`internal/data/concurrency_test.go`). Twenty goroutines writing
at once, proving `busy_timeout` turns SQLite's single-writer lock into a small
latency blip rather than a hard `SQLITE_BUSY` error. Two goroutines racing the
same update, proving exactly one wins and no write is lost. Readers running
during writes, which is the WAL journal mode doing its job. All under `-race`.

**Fuzzing.** Four targets, on the functions that parse untrusted bytes by hand:
`Runtime.UnmarshalJSON`, `Genres.Scan`, `readJSON`, and the email regexp. They
assert *properties* rather than outputs — "never panics", "anything accepted
round-trips unchanged", and for the email regexp, "an accepted address can never
contain a line break", which is what prevents SMTP header injection. Any crasher
found gets written to `testdata/fuzz/` and becomes a permanent regression test.

**Benchmarks.** Not for optimisation — as a tripwire and as documentation. Run
`make test/bench` and the "~250ms by design" claim about bcrypt below stops
being a claim. They also show the `LIMIT/OFFSET` pagination cost climbing on
deep pages, and what `json.MarshalIndent` costs on every response.

### A useful trick: `data.BcryptCost`

bcrypt at cost 12 takes ~250ms *by design*. A suite that creates dozens of users
would spend nearly all its time deliberately burning CPU — ours took 9.3
seconds. Both `TestMain`s drop the cost to `bcrypt.MinCost`, taking it to 0.8s.

This is safe because bcrypt stores the cost factor inside each hash, so
verification works regardless of the cost used to create it.

---

## Where this deviates from the books

Deliberate changes, all of them explained in comments at the site:

1. **`Mailer` is an interface** (declared in `cmd/api/main.go`), not the book's
   concrete struct. This is what lets tests inject a mock that records outgoing
   mail — the only way to get hold of an activation token in a test. It also
   follows the Go idiom of defining an interface where it's *used*, not where
   it's implemented.

2. **Emails are logged when no SMTP host is configured.** So you can clone this
   repo and register a user immediately, reading the activation token from the
   log, with no Mailtrap account.

3. **Migrations are embedded and applied on startup.** The book runs the
   `migrate` CLI by hand. Embedding with `//go:embed` gives a self-contained
   binary, lets tests build a real schema with no external tooling, and makes it
   impossible for the schema to drift from the binary. The CLI still works
   against `migrations/` if you prefer.

   > ⚠️ **One caveat, found while writing the tests for this.** Two processes
   > migrating the *same* database file at the *same moment* will wedge it:
   > golang-migrate marks `schema_migrations` dirty before applying a migration
   > and clears it afterwards, and its SQLite driver takes no advisory lock — so
   > the loser of the race leaves `Dirty database version 1. Fix and force
   > version.` behind, needing a manual `migrate force` to recover.
   >
   > In practice this only bites if you start two servers against one file
   > simultaneously (`make run/api` twice, or a restart overlapping a boot).
   > Sequential restarts are fine and are covered by
   > `TestMigrateUpAcrossRestarts`. It's a property of golang-migrate's SQLite
   > driver rather than of this code, so it's documented here rather than
   > worked around.

4. **`log/slog`** instead of the book's hand-rolled `jsonlog`.

5. **Version from VCS.** Go embeds the commit hash automatically since 1.18, so
   there's no need for the book's `-ldflags="-X main.version=..."`.

6. **expvar counters are package-level.** The book declares them inside the
   `metrics` middleware, which panics with `Reuse of exported var name` the
   second time `routes()` is called — which is exactly what a test suite does.

7. **`cmd/seed`** exists so the project is demoable in one command.

8. **No `remote/` deployment scripts.** The book's chapter 21 deploys to an
   Ubuntu box with Caddy; that's orthogonal to the SQLite port.

9. **Routing uses the standard library's `http.ServeMux`**, not the book's
   `julienschmidt/httprouter`. Go 1.22 added method-specific patterns and
   `{id}`-style wildcards to `ServeMux` itself, making the third-party router
   unnecessary — see [Why `net/http.ServeMux` instead of
   `httprouter`?](#why-nethttpservemux-instead-of-julienschmidthttprouter).

---

## Going further from here

Natural next steps, roughly in order of value:

- **Upgrade search to FTS5.** Create a virtual table and keep it in sync with
  triggers:

  ```sql
  CREATE VIRTUAL TABLE movies_fts USING fts5(title, content='movies', content_rowid='id');

  CREATE TRIGGER movies_ai AFTER INSERT ON movies BEGIN
      INSERT INTO movies_fts(rowid, title) VALUES (new.id, new.title);
  END;
  -- plus matching AFTER DELETE and AFTER UPDATE triggers
  ```

  Then `WHERE movies_fts MATCH ?` gives real ranked full-text search.

- **Split the connection pool** into a read pool and a single-connection write
  pool, to eliminate `SQLITE_BUSY` entirely rather than waiting it out.

- **Add a periodic job** calling `models.Tokens.DeleteExpired()` — the method is
  already written and tested.

- **Protect `/debug/vars`** behind authentication.

- **Add a permissions admin endpoint.** `data.ValidPermissionCode` exists for
  exactly this.

- **Constant-time login.** Currently an unregistered email returns slightly
  faster than a wrong password, because we skip the bcrypt comparison. Running a
  dummy comparison would close that timing side-channel.

- **Back up the database.** With SQLite this is `VACUUM INTO 'backup.db'`, which
  is safe to run against a live database — one of the real pleasures of using it.
