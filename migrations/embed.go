// Package migrations embeds the .sql migration files into the compiled binary.
//
// WHY EMBED THEM?
//
// The book runs migrations with the golang-migrate *command-line tool*, which
// reads the .sql files off disk. That works, but it means the binary alone
// isn't enough to stand the application up — you also need the migrations
// directory and the CLI installed next to it.
//
// By embedding the files with //go:embed we get three things:
//
//  1. `go build` produces a single self-contained binary that can migrate
//     itself on startup (see internal/db.MigrateUp).
//  2. Integration tests can spin up a fresh, fully-migrated database with no
//     external tooling — see internal/db.NewTestDB.
//  3. The schema can never drift out of sync with the binary that's running.
//
// The golang-migrate CLI still works against this directory if you prefer to
// drive migrations manually — see the `make db/migrations/*` targets.
package migrations

import "embed"

// FS holds the embedded migration files.
//
// The //go:embed directive must appear immediately above the variable, with no
// blank line between them. The pattern is relative to THIS directory, and
// embed.FS is read-only, so there's no risk of anything mutating it at runtime.
//
//go:embed *.sql
var FS embed.FS
