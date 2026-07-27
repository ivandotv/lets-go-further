// Command seed populates the database with a demo user and some sample movies,
// then prints a ready-to-use authentication token.
//
// This isn't from either book — in the book you'd grant permissions by hand
// with psql. It exists so that a fresh clone of this repo can go from `git
// clone` to a working authenticated request in one command, which makes the
// project much easier to explore.
//
// Usage:
//
//	go run ./cmd/seed -db-dsn=greenlight.db
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"greenlight/internal/data"
	"greenlight/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := flag.String("db-dsn", "greenlight.db", "SQLite database file path")
	email := flag.String("email", "demo@example.com", "Email address for the demo user")
	password := flag.String("password", "pa55word1234", "Password for the demo user")
	flag.Parse()

	database, err := db.OpenDB(db.Config{
		DSN:          *dsn,
		MaxOpenConns: 2,
		MaxIdleConns: 2,
		MaxIdleTime:  time.Minute,
	})
	if err != nil {
		return err
	}
	defer database.Close()

	// Make sure the schema exists — seeding a brand-new database should just
	// work without running the API server first.
	if err := db.MigrateUp(database); err != nil {
		return err
	}

	models := data.NewModels(database)

	// ── The demo user ────────────────────────────────────────────────────────
	//
	// Created already activated, and granted BOTH permissions, so it can
	// exercise every endpoint. (A user registering through the API normally
	// starts deactivated with only movies:read.)
	user := &data.User{
		Name:      "Demo User",
		Email:     *email,
		Activated: true,
	}

	if err := user.Password.Set(*password); err != nil {
		return err
	}

	err = models.Users.Insert(user)
	switch {
	case err == nil:
		fmt.Printf("created user %s\n", user.Email)

	case errors.Is(err, data.ErrDuplicateEmail):
		// Make the command re-runnable: if the user already exists, look them
		// up and carry on rather than failing.
		fmt.Printf("user %s already exists, reusing it\n", *email)

		user, err = models.Users.GetByEmail(*email)
		if err != nil {
			return err
		}

	default:
		return err
	}

	// AddForUser uses INSERT OR IGNORE, so re-granting is a no-op.
	err = models.Permissions.AddForUser(user.ID,
		data.PermissionMoviesRead, data.PermissionMoviesWrite)
	if err != nil {
		return err
	}

	// ── Sample movies ────────────────────────────────────────────────────────
	movies := []struct {
		title   string
		year    int32
		runtime data.Runtime
		genres  []string
	}{
		{"Casablanca", 1942, 102, []string{"drama", "romance", "war"}},
		{"The Breakfast Club", 1985, 96, []string{"drama", "comedy"}},
		{"Deadpool", 2016, 108, []string{"action", "comedy"}},
		{"Moana", 2016, 107, []string{"animation", "adventure"}},
		{"Black Panther", 2018, 134, []string{"action", "adventure", "sci-fi"}},
		{"Everything Everywhere All at Once", 2022, 139, []string{"action", "comedy", "sci-fi"}},
	}

	// Only insert the sample movies into an empty table, so re-running the
	// command doesn't pile up duplicates.
	filters := data.Filters{Page: 1, PageSize: 1, Sort: "id", SortSafelist: []string{"id"}}

	existing, _, err := models.Movies.GetAll("", nil, filters)
	if err != nil {
		return err
	}

	if len(existing) > 0 {
		fmt.Println("movies table is not empty, skipping sample movies")
	} else {
		for _, m := range movies {
			movie := &data.Movie{
				Title:   m.title,
				Year:    m.year,
				Runtime: m.runtime,
				Genres:  data.Genres(m.genres),
			}

			if err := models.Movies.Insert(movie); err != nil {
				return err
			}
		}

		fmt.Printf("inserted %d sample movies\n", len(movies))
	}

	// ── An authentication token to use straight away ─────────────────────────
	token, err := models.Tokens.New(user.ID, 24*time.Hour, data.ScopeAuthentication)
	if err != nil {
		return err
	}

	fmt.Printf(`
Demo user ready.

  email:    %s
  password: %s

Authentication token (valid 24h):

  %s

Try it:

  curl -H "Authorization: Bearer %s" localhost:4000/v1/movies

`, user.Email, *password, token.Plaintext, token.Plaintext)

	return nil
}
