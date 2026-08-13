package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"greenlight/internal/validator"
)

// Movie is the domain object, and doubles as the JSON representation.
//
// The struct tags control the JSON field names. A few things worth noting:
//
//   - `json:"-"` on CreatedAt hides it from responses entirely. It's an
//     internal bookkeeping field; clients don't need it.
//   - `omitempty` on Year/Runtime/Genres omits the field when it holds its zero
//     value. Handy for the empty-response case.
//   - Version is exposed so clients can see it change after an update, which
//     makes the optimistic-locking behaviour visible.
//
// Runtime is our custom type from runtime.go, so it serialises as "102 mins".
// Genres is our custom type from genres.go, so it round-trips through SQLite
// as a JSON string while still being a plain []string in Go and JSON.
type Movie struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"-"`
	Title     string    `json:"title"`
	Year      int32     `json:"year,omitempty"`
	Runtime   Runtime   `json:"runtime,omitempty"`
	Genres    Genres    `json:"genres,omitempty"`
	Version   int32     `json:"version"`
}

// ValidateMovie runs all the business rules for a Movie and records any
// failures on v.
//
// It's a plain function rather than a method so it can be called on a Movie
// that hasn't been persisted yet, and so it's trivially unit-testable.
func ValidateMovie(v *validator.Validator, movie *Movie) {
	v.Check(movie.Title != "", "title", "must be provided")
	v.Check(len(movie.Title) <= 500, "title", "must not be more than 500 bytes long")

	v.Check(movie.Year != 0, "year", "must be provided")
	v.Check(movie.Year >= 1888, "year", "must be greater than 1888")
	// 1888 is the year of Roundhay Garden Scene, the oldest surviving film.
	v.Check(movie.Year <= int32(time.Now().Year()), "year", "must not be in the future")

	v.Check(movie.Runtime != 0, "runtime", "must be provided")
	v.Check(movie.Runtime > 0, "runtime", "must be a positive integer")

	v.Check(movie.Genres != nil, "genres", "must be provided")
	v.Check(len(movie.Genres) >= 1, "genres", "must contain at least 1 genre")
	v.Check(len(movie.Genres) <= 5, "genres", "must not contain more than 5 genres")
	v.Check(validator.Unique(movie.Genres), "genres", "must not contain duplicate values")
}

// MovieModel wraps the connection pool and provides the CRUD methods for
// movies.
type MovieModel struct {
	DB *sql.DB
}

// Insert adds a new record and back-fills the system-generated fields
// (ID, CreatedAt, Version) onto the passed movie.
func (m MovieModel) Insert(movie *Movie) error {
	// Note the `?` placeholders. Postgres uses `$1, $2, ...`; SQLite (like
	// MySQL) uses positional `?`. This is the single most common change when
	// porting the book's queries.
	//
	// RETURNING is supported by SQLite 3.35+ (2021), so this pattern carries
	// over from the book unchanged — one round trip instead of two.
	query := `
		INSERT INTO movies (title, year, runtime, genres)
		VALUES (?, ?, ?, ?)
		RETURNING id, created_at, version`

	// Collect the arguments in a slice. This is purely for readability when
	// there are several of them — it lines the values up with the placeholders
	// above.
	args := []any{movie.Title, movie.Year, movie.Runtime, movie.Genres}

	// Every query gets a context with a timeout, so a pathological query can't
	// tie up a connection forever. `defer cancel()` is mandatory: without it
	// the context's resources leak until the deadline passes.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return m.DB.QueryRowContext(ctx, query, args...).Scan(&movie.ID, &movie.CreatedAt, &movie.Version)
}

// Get fetches a single movie by ID.
func (m MovieModel) Get(id int64) (*Movie, error) {
	// Our IDs are auto-incrementing and start at 1, so anything less than 1
	// can't exist. Short-circuiting here avoids a pointless database round trip
	// and gives the same 404 the client would get anyway.
	if id < 1 {
		return nil, ErrRecordNotFound
	}

	query := `
		SELECT id, created_at, title, year, runtime, genres, version
		FROM movies
		WHERE id = ?`

	var movie Movie

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, id).Scan(
		&movie.ID,
		&movie.CreatedAt,
		&movie.Title,
		&movie.Year,
		&movie.Runtime,
		&movie.Genres, // Genres.Scan turns the JSON text back into []string.
		&movie.Version,
	)
	if err != nil {
		// Translate the database-specific "no rows" into our own sentinel, so
		// callers never need to import database/sql.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRecordNotFound
		}
		return nil, err
	}

	return &movie, nil
}

// Update writes a modified movie back to the database.
//
// ── OPTIMISTIC CONCURRENCY CONTROL ───────────────────────────────────────────
//
// Consider two clients both editing movie 7 at the same time. Both GET it
// (version 3), both change a field, both PATCH. Without protection, the second
// write silently clobbers the first — a lost update.
//
// The fix: include `AND version = ?` in the WHERE clause with the version we
// read, and bump the version on every write. The first update matches and sets
// version to 4. The second update's WHERE no longer matches anything, so it
// affects zero rows — we detect that and return ErrEditConflict, and the
// handler turns it into a 409.
//
// It's called "optimistic" because we don't take any locks; we assume conflicts
// are rare and just detect them when they happen.
func (m MovieModel) Update(movie *Movie) error {
	query := `
		UPDATE movies
		SET title = ?, year = ?, runtime = ?, genres = ?, version = version + 1
		WHERE id = ? AND version = ?
		RETURNING version`

	args := []any{
		movie.Title,
		movie.Year,
		movie.Runtime,
		movie.Genres,
		movie.ID,
		movie.Version, // the version we believe is current
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := m.DB.QueryRowContext(ctx, query, args...).Scan(&movie.Version)
	if err != nil {
		// No rows came back, which means the WHERE clause matched nothing.
		// Either the record was deleted, or its version moved on without us.
		// Both are edit conflicts from the client's point of view.
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEditConflict
		}
		return err
	}

	return nil
}

// Delete removes a movie by ID.
func (m MovieModel) Delete(id int64) error {
	if id < 1 {
		return ErrRecordNotFound
	}

	query := `DELETE FROM movies WHERE id = ?`

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// ExecContext (not QueryContext) because DELETE returns no rows.
	result, err := m.DB.ExecContext(ctx, query, id)
	if err != nil {
		return err
	}

	// A DELETE that matches nothing is not an error as far as SQL is concerned,
	// so we have to check the affected-row count ourselves to distinguish
	// "deleted" from "wasn't there".
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return ErrRecordNotFound
	}

	return nil
}

// GetAll returns a page of movies matching the given title and genres filters,
// along with pagination metadata.
//
// An empty title matches everything; an empty genres slice matches everything.
// That "empty means no filter" behaviour is implemented in SQL rather than by
// building the query string conditionally, which keeps us to a single prepared
// statement shape.
func (m MovieModel) GetAll(title string, genres []string, filters Filters) ([]*Movie, Metadata, error) {
	// ── Sorting ──────────────────────────────────────────────────────────────
	//
	// The ORDER BY column cannot be a placeholder — SQL simply doesn't allow
	// parameterising identifiers — so it gets interpolated into the string.
	// That is only safe because sortColumn() checks the value against the
	// safelist and panics otherwise. Never interpolate anything else.
	orderBy := filters.sortColumn()

	// Sort titles case-insensitively. SQLite's default BINARY collation sorts
	// by byte value, which puts every uppercase letter before every lowercase
	// one ("Zulu" before "apple") — not what a human expects from an
	// alphabetical movie list. NOCASE also matches the collation on
	// idx_movies_title, so the index can actually satisfy the sort.
	if orderBy == "title" {
		orderBy += " COLLATE NOCASE"
	}

	// ── Genre containment ────────────────────────────────────────────────────
	//
	// The book uses Postgres array containment: `genres @> $2`, meaning "the
	// movie's genres include ALL of the requested ones".
	//
	// SQLite has no arrays, but it does have the JSON1 extension (built in
	// since 3.38). json_each() explodes a JSON array into a virtual table of
	// rows, so we can express containment as:
	//
	//   "the number of requested genres that are present in this movie's
	//    genres equals the total number of requested genres"
	//
	// The leading json_array_length(?) = 0 handles "no filter supplied".
	//
	// Doing it this way keeps a FIXED number of placeholders regardless of how
	// many genres were requested, so we don't have to build SQL dynamically.
	query := fmt.Sprintf(`
		SELECT count(*) OVER(), id, created_at, title, year, runtime, genres, version
		FROM movies
		WHERE (? = '' OR title LIKE '%%' || ? || '%%')
		AND (
			json_array_length(?) = 0
			OR (
				SELECT count(*)
				FROM json_each(?) AS requested
				WHERE EXISTS (
					SELECT 1 FROM json_each(movies.genres) AS mg
					WHERE mg.value = requested.value
				)
			) = json_array_length(?)
		)
		ORDER BY %s %s, id ASC
		LIMIT ? OFFSET ?`, orderBy, filters.sortDirection())

	// `count(*) OVER()` is a window function: it computes the total number of
	// rows matching the WHERE clause *before* LIMIT/OFFSET are applied, and
	// repeats that total on every returned row. That's how we get the total
	// record count for pagination without a second COUNT query.
	//
	// The secondary `, id ASC` in the ORDER BY is important: without a
	// tiebreaker, rows with equal sort values can come back in arbitrary (and
	// inconsistent) order between pages, so a record could appear twice or not
	// at all as the client pages through.

	// Encode the requested genres as a JSON array so json_each can consume it.
	// Genres.Value() already does exactly this, and handles the nil case.
	genresJSON, err := Genres(genres).Value()
	if err != nil {
		return nil, Metadata{}, err
	}

	// The repeated arguments line up with the repeated placeholders above.
	// (SQLite does support named parameters, which would avoid the repetition,
	// but positional args keep this closest to the book's code.)
	args := []any{
		title, title,
		genresJSON, genresJSON, genresJSON,
		filters.limit(), filters.offset(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := m.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, Metadata{}, err
	}
	// Closing the rows releases the underlying connection back to the pool.
	// Forgetting this leaks connections until the pool is exhausted and the
	// application deadlocks — one of the nastiest bugs to track down.
	defer func() { _ = rows.Close() }()

	totalRecords := 0
	// Initialise as an empty (non-nil) slice, so that "no results" encodes as
	// `[]` in JSON rather than `null`.
	movies := []*Movie{}

	for rows.Next() {
		var movie Movie

		err := rows.Scan(
			&totalRecords, // the same value on every row
			&movie.ID,
			&movie.CreatedAt,
			&movie.Title,
			&movie.Year,
			&movie.Runtime,
			&movie.Genres,
			&movie.Version,
		)
		if err != nil {
			return nil, Metadata{}, err
		}

		movies = append(movies, &movie)
	}

	// rows.Next() returns false both on normal completion AND on error, so we
	// must check rows.Err() afterwards. Skipping this check means silently
	// returning a truncated result set when something goes wrong mid-iteration.
	if err := rows.Err(); err != nil {
		return nil, Metadata{}, err
	}

	metadata := calculateMetadata(totalRecords, filters.Page, filters.PageSize)

	return movies, metadata, nil
}
