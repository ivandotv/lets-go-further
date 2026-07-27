package main

import (
	"errors"
	"net/http"
	"time"

	"greenlight/internal/data"
	"greenlight/internal/validator"
)

// createAuthenticationTokenHandler exchanges an email and password for a
// bearer token.
//
//	POST /v1/tokens/authentication
//	{"email": "alice@example.com", "password": "pa55word1234"}
//
// ── WHY STATEFUL TOKENS RATHER THAN JWTs? ────────────────────────────────────
//
// The book weighs this up carefully and lands on database-backed tokens, and
// this project follows suit. The decisive advantage is REVOCATION: because
// every token has a row, deleting the row logs the user out instantly.
//
// A JWT is self-contained and self-verifying, which is great for throughput —
// no database lookup per request — but it means a leaked JWT stays valid until
// it expires, and there's nothing you can do about it short of maintaining a
// deny-list, at which point you've reinvented the database lookup you were
// trying to avoid.
//
// For an API of this size, the extra lookup is trivial and the operational
// simplicity is worth a lot.
func (app *application) createAuthenticationTokenHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	// Validate the shape of the credentials before hitting the database.
	// Note we use ValidatePasswordPlaintext here so an obviously-invalid
	// password (empty, or 300 bytes) is rejected without a bcrypt comparison.
	v := validator.New()
	data.ValidateEmail(v, input.Email)
	data.ValidatePasswordPlaintext(v, input.Password)

	if !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	user, err := app.models.Users.GetByEmail(input.Email)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			// Deliberately the SAME response as a wrong password. Distinguishing
			// them would let an attacker enumerate which email addresses have
			// accounts.
			//
			// (A fully rigorous implementation would also run a dummy bcrypt
			// comparison here, because returning early is measurably faster
			// and leaks the same information through timing. That's beyond
			// what the book does, but it's worth knowing about.)
			app.invalidCredentialsResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	match, err := user.Password.Matches(input.Password)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	if !match {
		app.invalidCredentialsResponse(w, r)
		return
	}

	// A 24-hour lifetime. Short enough that a leaked token stops being useful
	// fairly quickly; long enough that users aren't re-authenticating all day.
	token, err := app.models.Tokens.New(user.ID, 24*time.Hour, data.ScopeAuthentication)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	// 201 Created — we've created a new token resource.
	//
	// This response is the ONLY time the plaintext token exists outside the
	// client. We store just its hash, so if the user loses it they must
	// authenticate again to get a new one.
	if err := app.writeJSON(w, http.StatusCreated, envelope{"authentication_token": token}, nil); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
