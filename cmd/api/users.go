package main

import (
	"errors"
	"net/http"
	"time"

	"greenlight/internal/data"
	"greenlight/internal/validator"
)

// registerUserHandler creates a new (inactive) user account and emails them an
// activation token.
//
//	POST /v1/users
//	{"name": "Alice", "email": "alice@example.com", "password": "pa55word1234"}
func (app *application) registerUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	user := &data.User{
		Name:  input.Name,
		Email: input.Email,
		// New accounts start deactivated. The user proves they control the
		// email address by returning the token we send there.
		Activated: false,
	}

	// Set() hashes the password with bcrypt. Note it's the ONLY way to
	// populate the password field — see the type's comment in data/users.go.
	if err := user.Password.Set(input.Password); err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	v := validator.New()
	if data.ValidateUser(v, user); !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	if err := app.models.Users.Insert(user); err != nil {
		switch {
		case errors.Is(err, data.ErrDuplicateEmail):
			// Report this as a field-level validation error rather than a
			// generic 500, so the client can highlight the email input.
			//
			// SECURITY TRADE-OFF: this confirms to an attacker that a given
			// address is registered. The alternative — pretending success and
			// emailing "someone tried to register with your address" — is more
			// private but much more confusing for legitimate users. The book
			// makes the same call.
			v.AddError("email", "a user with this email address already exists")
			app.failedValidationResponse(w, r, v.Errors)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	// Give every new user read access to movies. Write access is granted
	// separately (by an admin, in a real system).
	if err := app.models.Permissions.AddForUser(user.ID, data.PermissionMoviesRead); err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	// Generate an activation token valid for 3 days.
	token, err := app.models.Tokens.New(user.ID, 3*24*time.Hour, data.ScopeActivation)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	// ── Send the email in the background ─────────────────────────────────────
	//
	// SMTP round trips take seconds. There's no reason to make the client wait
	// for one, so this runs in a tracked goroutine (see app.background for why
	// that helper exists rather than a bare `go`).
	//
	// The closure CAPTURES token and user by reference. That's safe here
	// because nothing else mutates them after this point — but it's exactly
	// the kind of thing to watch for when using goroutines inside a handler,
	// since the handler returns immediately and its variables would otherwise
	// be reused.
	app.background(func() {
		data := map[string]any{
			"activationToken": token.Plaintext,
			"userID":          user.ID,
			"name":            user.Name,
		}

		if err := app.mailer.Send(user.Email, "user_welcome.tmpl", data); err != nil {
			// We can't return an error from a background goroutine — the
			// response has already gone out. Logging is all we can do.
			app.logger.Error("failed to send welcome email", "error", err, "recipient", user.Email)
		}
	})

	// 202 Accepted, not 201 Created: the request has been accepted for
	// processing but the work (sending the email) isn't finished yet.
	if err := app.writeJSON(w, http.StatusAccepted, envelope{"user": user}, nil); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

// activateUserHandler activates an account using the token that was emailed.
//
//	PUT /v1/users/activated
//	{"token": "Y3QMGX3PJ3WLRL2YRTQGQ6KRHU"}
//
// PUT rather than POST because activation is idempotent in effect: the
// resource being modified is the user's "activated" state, and we're setting it
// to a known value.
func (app *application) activateUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TokenPlaintext string `json:"token"`
	}

	if err := app.readJSON(w, r, &input); err != nil {
		app.badRequestResponse(w, r, err)
		return
	}

	v := validator.New()
	if data.ValidateTokenPlaintext(v, input.TokenPlaintext); !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}

	// Look the user up by the token. This also verifies scope and expiry.
	user, err := app.models.Users.GetForToken(data.ScopeActivation, input.TokenPlaintext)
	if err != nil {
		switch {
		case errors.Is(err, data.ErrRecordNotFound):
			v.AddError("token", "invalid or expired activation token")
			app.failedValidationResponse(w, r, v.Errors)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	user.Activated = true

	// Update uses optimistic locking, so two simultaneous activation requests
	// can't both succeed and corrupt the version counter.
	if err := app.models.Users.Update(user); err != nil {
		switch {
		case errors.Is(err, data.ErrEditConflict):
			app.editConflictResponse(w, r)
		default:
			app.serverErrorResponse(w, r, err)
		}
		return
	}

	// Burn all this user's activation tokens now they've been used. Without
	// this, the token would keep working for its full 3-day life — and anyone
	// who saw the email (or a log, or a shoulder-surf) could reuse it.
	if err := app.models.Tokens.DeleteAllForUser(data.ScopeActivation, user.ID); err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	if err := app.writeJSON(w, http.StatusOK, envelope{"user": user}, nil); err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
