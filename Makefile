# ==================================================================================== #
# This Makefile follows the pattern from chapter 19 of "Let's Go Further".
#
# A note on syntax, since Makefiles are their own little world:
#
#   .PHONY: target   declares that `target` is a command, not a file to build.
#                    Without it, `make audit` would do nothing if a file called
#                    "audit" happened to exist.
#
#   ## comments      lines starting with ## are picked up by the `help` target
#                    below, which is how `make help` documents itself.
#
#   @command         the @ suppresses echoing the command before running it.
#
#   ${VAR}           a variable. Ones defined with ?= can be overridden from the
#                    command line, e.g. `make run/api port=5000`.
# ==================================================================================== #

# The database file. Override with: make run/api db_dsn=/tmp/other.db
db_dsn ?= greenlight.db

# Where the migration .sql files live.
migrations_path ?= ./migrations

# ==================================================================================== #
# HELPERS
# ==================================================================================== #

## help: print this help message
.PHONY: help
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

## confirm: ask for confirmation before a destructive action
.PHONY: confirm
confirm:
	@echo -n 'Are you sure? [y/N] ' && read ans && [ $${ans:-N} = y ]

# ==================================================================================== #
# DEVELOPMENT
# ==================================================================================== #

## run/api: run the API server
##
## Migrations run automatically on startup, so this works against an empty
## directory with no setup at all.
.PHONY: run/api
run/api:
	go run ./cmd/api -db-dsn=${db_dsn}

## run/seed: create a demo user with sample data and print an auth token
.PHONY: run/seed
run/seed:
	go run ./cmd/seed -db-dsn=${db_dsn}

## db/shell: open a sqlite3 shell against the database
.PHONY: db/shell
db/shell:
	sqlite3 ${db_dsn}

## db/migrations/new name=$1: create a new pair of migration files
##
## Requires the golang-migrate CLI:
##   go install -tags 'sqlite' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
##
## Note you only need the CLI to CREATE migrations. Applying them needs nothing
## extra, because the .sql files are embedded in the binary and applied on
## startup (see internal/db.MigrateUp).
.PHONY: db/migrations/new
db/migrations/new:
	@echo 'Creating migration files for ${name}...'
	migrate create -seq -ext=.sql -dir=${migrations_path} ${name}

## db/migrations/up: apply all up migrations manually
##
## Normally unnecessary — the API applies them itself on startup. This target
## is here for when you want to migrate without running the server.
.PHONY: db/migrations/up
db/migrations/up:
	@echo 'Running up migrations...'
	migrate -path=${migrations_path} -database="sqlite://${db_dsn}" up

## db/migrations/down: roll back all migrations
.PHONY: db/migrations/down
db/migrations/down: confirm
	migrate -path=${migrations_path} -database="sqlite://${db_dsn}" down

## db/migrations/version: print the current migration version
.PHONY: db/migrations/version
db/migrations/version:
	migrate -path=${migrations_path} -database="sqlite://${db_dsn}" version

## db/reset: delete the database file and start over
.PHONY: db/reset
db/reset: confirm
	@rm -f ${db_dsn} ${db_dsn}-wal ${db_dsn}-shm
	@echo 'Database removed. It will be recreated and migrated on next run.'

# ==================================================================================== #
# QUALITY CONTROL
# ==================================================================================== #

## audit: format, vet, and test everything
.PHONY: audit
audit: tidy
	@echo 'Formatting code...'
	go fmt ./...
	@echo 'Vetting code...'
	go vet ./...
	@echo 'Running tests...'
	go test -race -vet=off ./...

## tidy: tidy and verify module dependencies
.PHONY: tidy
tidy:
	@echo 'Tidying module dependencies...'
	go mod tidy
	@echo 'Verifying module dependencies...'
	go mod verify

## test: run all tests
.PHONY: test
test:
	go test -race ./...

## test/short: run only the fast unit tests, skipping database-backed ones
.PHONY: test/short
test/short:
	go test -short ./...

## test/cover: run tests and open the HTML coverage report
.PHONY: test/cover
test/cover:
	go test -coverprofile=/tmp/coverage.out ./...
	go tool cover -html=/tmp/coverage.out

# ==================================================================================== #
# BUILD
# ==================================================================================== #

# Linker flags:
#   -s  strip the symbol table
#   -w  strip DWARF debug information
#
# Together these cut roughly 25% off the binary size. We do NOT need the book's
# `-X main.version=...` trick, because Go embeds the VCS revision automatically
# since 1.18 — see vcsRevision() in cmd/api/main.go.
linker_flags = '-s -w'

## build/api: build the API server for the current platform
.PHONY: build/api
build/api:
	@echo 'Building cmd/api...'
	go build -ldflags=${linker_flags} -o=./bin/api ./cmd/api
	go build -ldflags=${linker_flags} -o=./bin/seed ./cmd/seed

## build/linux: cross-compile the API server for linux/amd64
##
## This is where the pure-Go SQLite driver earns its keep: with the cgo-based
## mattn/go-sqlite3 you'd need a linux C cross-toolchain. Here it's one command
## with no extra setup.
.PHONY: build/linux
build/linux:
	@echo 'Building for linux/amd64...'
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags=${linker_flags} -o=./bin/linux_amd64/api ./cmd/api
