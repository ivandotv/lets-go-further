package main

import (
	"errors"
	"fmt"
	"net/http"

	"greenlight/internal/data"
	"greenlight/internal/validator"
)

// createMovieHandler adds a new movie.
//
//	POST /v1/movies
//	{"title": "Moana", "year": 2016, "runtime": "107 mins", "genres": ["animation"]}
func (app *application) createMovieHandler(w http.ResponseWriter, r *http.Request) {
	// ── Why an anonymous struct instead of decoding into data.Movie? ──────────
	//
	// Decoding straight into a data.Movie would let a client set fields they
	// have no business setting — id, version, created_at. That's a mass
	// assignment vulnerability.
	//
	// This input struct is the API's *contract*: it lists exactly which fields
	// a client may supply, and nothing else can get through.
	var input struct {
		Title   string       `json:"title"`
		Year    int32        `json:"year"`
		Runtime data.Runtime `json:"runtime"`
		Genres  []string     `json:"genres"`
	}

	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	// Copy the permitted fields across into a real Movie.
	movie := &data.Movie{
		Title:   input.Title,
		Year:    input.Year,
		Runtime: input.Runtime,
		Genres:  data.Genres(input.Genres),
	}

	v := validator.New()

	// The `if x; !cond` form runs ValidateMovie and then tests v.Valid() —
	// a compact Go idiom that keeps the validator's scope tight.
	if data.ValidateMovie(v, movie); !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	if err := app.models.Movies.Insert(movie); err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	// A 201 Created should tell the client where the new resource lives.
	headers := make(http.Header)
	headers.Set("Location", fmt.Sprintf("/v1/movies/%d", movie.ID))

	if err := app.writeJSON(w, http.StatusCreated, envelope{"movie": movie}, headers); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

// showMovieHandler returns a single movie.
//
//	GET /v1/movies/:id
func (app *application) showMovieHandler(w http.ResponseWriter, r *http.Request) {
	id, err := app.readIDParam(r)
	if err != nil {
		app.notFoundResponse(w, r)
		return
	}

	movie, err := app.models.Movies.Get(id)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			app.notFoundResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	if err := app.writeJSON(w, http.StatusOK, envelope{"movie": movie}, nil); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

// updateMovieHandler partially updates a movie.
//
//	PATCH /v1/movies/:id
//
// ── WHY POINTERS IN THE INPUT STRUCT ─────────────────────────────────────────
//
// This is a PATCH (partial update), so a client should be able to send just
// {"year": 1985} and leave everything else alone.
//
// With non-pointer fields we couldn't tell "the client sent year: 0" apart from
// "the client didn't send year at all" — both leave the field at its zero
// value. A *int32 distinguishes them: nil means absent, non-nil means supplied.
//
// (Switch these to non-pointers and route PUT instead if you want full-replace
// semantics.)
func (app *application) updateMovieHandler(w http.ResponseWriter, r *http.Request) {
	id, err := app.readIDParam(r)
	if err != nil {
		app.notFoundResponse(w, r)
		return
	}

	// Fetch the existing record first — we need its current Version for the
	// optimistic-locking check, and its current values for any field the
	// client doesn't supply.
	movie, err := app.models.Movies.Get(id)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			app.notFoundResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	var input struct {
		Title   *string       `json:"title"`
		Year    *int32        `json:"year"`
		Runtime *data.Runtime `json:"runtime"`
		Genres  []string      `json:"genres"`
		// Genres is already a slice, and a nil slice is itself distinguishable
		// from an empty one, so it needs no extra pointer.
	}

	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	// Apply only the fields that were actually supplied.
	if input.Title != nil {
		movie.Title = *input.Title
	}
	if input.Year != nil {
		movie.Year = *input.Year
	}
	if input.Runtime != nil {
		movie.Runtime = *input.Runtime
	}
	if input.Genres != nil {
		movie.Genres = data.Genres(input.Genres)
	}

	v := validator.New()
	if data.ValidateMovie(v, movie); !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	if err := app.models.Movies.Update(movie); err != nil {
		switch {
		// Someone else changed this record between our Get and our Update.
		case errors.Is(err, data.ErrEditConflict):
			app.editConflictResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	if err := app.writeJSON(w, http.StatusOK, envelope{"movie": movie}, nil); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

// deleteMovieHandler removes a movie.
//
//	DELETE /v1/movies/:id
func (app *application) deleteMovieHandler(w http.ResponseWriter, r *http.Request) {
	id, err := app.readIDParam(r)
	if err != nil {
		app.notFoundResponse(w, r)
		return
	}

	if err := app.models.Movies.Delete(id); err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			app.notFoundResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	if err := app.writeJSON(w, http.StatusOK, envelope{"message": "movie successfully deleted"}, nil); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

// listMoviesHandler returns a filtered, sorted, paginated list of movies.
//
//	GET /v1/movies?title=black&genres=action,adventure&page=1&page_size=20&sort=-year
func (app *application) listMoviesHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title  string
		Genres []string
		data.Filters
		// Embedding data.Filters promotes its fields, so we can write
		// input.Page rather than input.Filters.Page.
	}

	v := validator.New()

	// Parse the query string once and reuse it — r.URL.Query() re-parses on
	// every call.
	qs := r.URL.Query()

	input.Title = app.readString(qs, "title", "")
	input.Genres = app.readCSV(qs, "genres", []string{})

	input.Filters.Page = app.readInt(qs, "page", 1, v)
	input.Filters.PageSize = app.readInt(qs, "page_size", 20, v)
	input.Filters.Sort = app.readString(qs, "sort", "id")

	// The safelist is set HERE, by us — never from client input. It's what
	// makes interpolating the sort column into the SQL safe. See
	// data.Filters.sortColumn.
	input.Filters.SortSafelist = []string{
		"id", "title", "year", "runtime",
		"-id", "-title", "-year", "-runtime",
	}

	if data.ValidateFilters(v, input.Filters); !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	movies, metadata, err := app.models.Movies.GetAll(input.Title, input.Genres, input.Filters)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	err = app.writeJSON(w, http.StatusOK, envelope{"movies": movies, "metadata": metadata}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
