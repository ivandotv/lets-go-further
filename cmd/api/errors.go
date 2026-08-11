package main

import (
	"fmt"
	"net/http"
)

// This file centralises every error response the API can produce.
//
// Two benefits of doing it this way rather than writing http.Error calls inline:
//
//  1. Error responses have exactly the same JSON envelope shape as success
//     responses, so clients can parse them with the same code path.
//  2. Changing the format (adding an error code, say) is a change in one file.

// logError records an error along with the request that caused it.
//
// Including the method and URI is what turns "something failed" into an
// actionable log line.
func (app *application) logError(r *http.Request, err error) {
	app.logger.Error(err.Error(),
		"method", r.Method,
		"uri", r.URL.RequestURI(),
	)
}

// errorResponse writes a JSON error at the given status code.
//
// `message` is `any` rather than `string` so callers can pass either a plain
// string or the validator's map[string]string of field errors.
func (app *application) errorResponse(w http.ResponseWriter, r *http.Request, status int, message any) {
	env := envelope{"error": message}

	err := app.writeJSON(w, status, env, nil)
	if err != nil {
		// If we can't even write the error response, log it and fall back to
		// an empty 500. There's nothing more useful we can do at this point.
		app.logError(r, err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// serverErrorResponse handles unexpected, in-our-code problems (500).
//
// Note that the client is told nothing specific. Leaking internal errors —
// SQL text, file paths, driver messages — is an information disclosure
// vulnerability. The detail goes to the log, where operators can see it.
func (app *application) serverErrorResponse(w http.ResponseWriter, r *http.Request, err error) {
	app.logError(r, err)

	message := "the server encountered a problem and could not process your request"
	app.errorResponse(w, r, http.StatusInternalServerError, message)
}

// notFoundResponse is sent when a resource doesn't exist (404).
func (app *application) notFoundResponse(w http.ResponseWriter, r *http.Request) {
	message := "the requested resource could not be found"
	app.errorResponse(w, r, http.StatusNotFound, message)
}

// methodNotAllowedResponse is sent when the URL exists but not for this HTTP
// method (405). routes.go wires this in as a fallback pattern for each path.
func (app *application) methodNotAllowedResponse(w http.ResponseWriter, r *http.Request) {
	message := fmt.Sprintf("the %s method is not supported for this resource", r.Method)
	app.errorResponse(w, r, http.StatusMethodNotAllowed, message)
}

// badRequestResponse is sent when we can't even parse the request (400).
func (app *application) badRequestResponse(w http.ResponseWriter, r *http.Request, err error) {
	app.errorResponse(w, r, http.StatusBadRequest, err.Error())
}

// failedValidationResponse is sent when the request parsed fine but the
// contents broke our business rules (422 Unprocessable Entity).
//
// 422 rather than 400 is the meaningful distinction: 400 means "I couldn't
// understand this", 422 means "I understood it and it's wrong".
//
// The errors map goes straight into the response, so a client sees exactly
// which fields to fix.
func (app *application) failedValidationResponse(w http.ResponseWriter, r *http.Request, errors map[string]string) {
	app.errorResponse(w, r, http.StatusUnprocessableEntity, errors)
}

// editConflictResponse is sent when optimistic locking detected a concurrent
// update (409 Conflict). See data.MovieModel.Update.
func (app *application) editConflictResponse(w http.ResponseWriter, r *http.Request) {
	message := "unable to update the record due to an edit conflict, please try again"
	app.errorResponse(w, r, http.StatusConflict, message)
}

// rateLimitExceededResponse is sent when a client exceeds its request quota
// (429 Too Many Requests).
func (app *application) rateLimitExceededResponse(w http.ResponseWriter, r *http.Request) {
	message := "rate limit exceeded"
	app.errorResponse(w, r, http.StatusTooManyRequests, message)
}

// invalidCredentialsResponse is sent when an email/password pair doesn't match
// (401).
//
// The message is deliberately vague about WHICH part was wrong. Saying "no
// such user" would let an attacker enumerate registered email addresses.
func (app *application) invalidCredentialsResponse(w http.ResponseWriter, r *http.Request) {
	message := "invalid authentication credentials"
	app.errorResponse(w, r, http.StatusUnauthorized, message)
}

// invalidAuthenticationTokenResponse is sent when the bearer token is missing,
// malformed or expired (401).
//
// The `WWW-Authenticate: Bearer` header is required by RFC 7235 on a 401: it
// tells the client which authentication scheme to use for a retry.
func (app *application) invalidAuthenticationTokenResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")

	message := "invalid or missing authentication token"
	app.errorResponse(w, r, http.StatusUnauthorized, message)
}

// authenticationRequiredResponse is sent when an anonymous user hits a
// protected endpoint (401).
func (app *application) authenticationRequiredResponse(w http.ResponseWriter, r *http.Request) {
	message := "you must be authenticated to access this resource"
	app.errorResponse(w, r, http.StatusUnauthorized, message)
}

// inactiveAccountResponse is sent when an authenticated but unactivated user
// hits a protected endpoint (403).
//
// 403 rather than 401 because the credentials were fine — the account simply
// isn't permitted. Retrying with different credentials wouldn't help.
func (app *application) inactiveAccountResponse(w http.ResponseWriter, r *http.Request) {
	message := "your user account must be activated to access this resource"
	app.errorResponse(w, r, http.StatusForbidden, message)
}

// notPermittedResponse is sent when the user lacks the required permission
// (403).
func (app *application) notPermittedResponse(w http.ResponseWriter, r *http.Request) {
	message := "your user account doesn't have the necessary permissions to access this resource"
	app.errorResponse(w, r, http.StatusForbidden, message)
}
