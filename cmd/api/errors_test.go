package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"greenlight/internal/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// ERROR RESPONSES
// ─────────────────────────────────────────────────────────────────────────────
//
// Most of these helpers are reached incidentally by the handler tests, but a
// few weren't reached at all — editConflictResponse in particular, because the
// only test that produced an edit conflict asserted at the model layer.
//
// Testing them directly is worthwhile beyond the coverage number: these
// responses ARE the API's error contract. The status codes and the shape of the
// envelope are what clients program against, and the wording of two of them
// (invalidCredentials, notFound) is a deliberate security choice rather than a
// stylistic one.
//
// ── httptest.NewRecorder, AND WHY THERE'S NO SERVER HERE ─────────────────────
//
// These call the handler functions directly rather than going over HTTP, using
// two standard-library helpers:
//
//	httptest.NewRecorder()          an http.ResponseWriter that records what was
//	                                written instead of sending it anywhere
//	httptest.NewRequest(...)        an *http.Request built for testing, which
//	                                panics on bad input rather than returning an
//	                                error — fine in a test, and it saves a check
//
// rr.Result() then gives you an *http.Response to assert on. Always close its
// Body: it's an io.ReadCloser like any other, and `go vet` won't remind you.
//
// The other style — a real server via httptest.NewServer — is in
// testutils_test.go and is used for anything involving routing or middleware.
// Recorders are right here because these functions ARE the unit under test;
// there's no chain to exercise.

// newErrorTestApp builds an application with a captured logger, so tests can
// assert on what was logged as well as what was sent.
func newErrorTestApp(logOutput io.Writer) *application {
	if logOutput == nil {
		logOutput = io.Discard
	}

	return &application{
		logger: slog.New(slog.NewTextHandler(logOutput, nil)),
	}
}

// TestErrorResponses walks every helper and pins its status code and message.
func TestErrorResponses(t *testing.T) {
	app := newErrorTestApp(nil)

	tests := []struct {
		name       string
		call       func(w http.ResponseWriter, r *http.Request)
		wantStatus int
		wantBody   string
	}{
		{
			name:       "notFound",
			call:       app.notFoundResponse,
			wantStatus: http.StatusNotFound,
			wantBody:   "the requested resource could not be found",
		},
		{
			name:       "methodNotAllowed",
			call:       app.methodNotAllowedResponse,
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "the GET method is not supported for this resource",
		},
		{
			name:       "editConflict",
			call:       app.editConflictResponse,
			wantStatus: http.StatusConflict,
			wantBody:   "unable to update the record due to an edit conflict, please try again",
		},
		{
			name:       "rateLimitExceeded",
			call:       app.rateLimitExceededResponse,
			wantStatus: http.StatusTooManyRequests,
			wantBody:   "rate limit exceeded",
		},
		{
			name:       "invalidCredentials",
			call:       app.invalidCredentialsResponse,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "invalid authentication credentials",
		},
		{
			name:       "invalidAuthenticationToken",
			call:       app.invalidAuthenticationTokenResponse,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "invalid or missing authentication token",
		},
		{
			name:       "authenticationRequired",
			call:       app.authenticationRequiredResponse,
			wantStatus: http.StatusUnauthorized,
			wantBody:   "you must be authenticated to access this resource",
		},
		{
			name:       "inactiveAccount",
			call:       app.inactiveAccountResponse,
			wantStatus: http.StatusForbidden,
			wantBody:   "your user account must be activated to access this resource",
		},
		{
			name:       "notPermitted",
			call:       app.notPermittedResponse,
			wantStatus: http.StatusForbidden,
			wantBody:   "your user account doesn't have the necessary permissions to access this resource",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/v1/movies/1", nil)

			tt.call(rr, r)

			rs := rr.Result()
			defer func() { _ = rs.Body.Close() }()

			assert.Equal(t, rs.StatusCode, tt.wantStatus)
			assert.Equal(t, rs.Header.Get("Content-Type"), "application/json")

			// Every error must use the same envelope shape, so a client can
			// parse successes and failures with one code path.
			var env struct {
				Error string `json:"error"`
			}

			body, err := io.ReadAll(rs.Body)
			assert.NilError(t, err)

			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("error response was not the expected envelope: %s", body)
			}

			assert.Equal(t, env.Error, tt.wantBody)
		})
	}
}

// TestInvalidAuthenticationTokenSetsWWWAuthenticate checks the one error
// response that carries a header.
//
// RFC 7235 requires a WWW-Authenticate header on a 401 — it's how a client
// learns which scheme to retry with. Without it, a compliant client has no way
// to know this is a bearer-token API.
func TestInvalidAuthenticationTokenSetsWWWAuthenticate(t *testing.T) {
	app := newErrorTestApp(nil)

	rr := httptest.NewRecorder()
	app.invalidAuthenticationTokenResponse(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, rr.Result().Header.Get("WWW-Authenticate"), "Bearer")
}

// TestServerErrorResponseHidesDetail is a security test.
//
// The error passed in is logged in full, but the CLIENT must be told nothing
// specific. Leaking SQL text, file paths or driver messages into a response
// body is an information-disclosure vulnerability, and it's an easy one to
// reintroduce by "improving" the error message.
func TestServerErrorResponseHidesDetail(t *testing.T) {
	var logBuf bytes.Buffer

	app := newErrorTestApp(&logBuf)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/movies/1", nil)

	secret := `pq: relation "users" does not exist; dsn=file:/srv/secret.db`

	app.serverErrorResponse(rr, r, assertableError(secret))

	rs := rr.Result()
	defer func() { _ = rs.Body.Close() }()

	body, err := io.ReadAll(rs.Body)
	assert.NilError(t, err)

	assert.Equal(t, rs.StatusCode, http.StatusInternalServerError)
	assert.StringContains(t, string(body), "the server encountered a problem")

	// The detail must not appear in the response...
	if strings.Contains(string(body), "secret.db") || strings.Contains(string(body), "relation") {
		t.Errorf("internal error detail leaked to the client: %s", body)
	}

	// ...but it must appear in the log, along with the request that caused it.
	logged := logBuf.String()
	assert.StringContains(t, logged, "secret.db")
	assert.StringContains(t, logged, "/v1/movies/1")
	assert.StringContains(t, logged, "GET")
}

// TestFailedValidationResponse checks that the validator's field-error map is
// passed through as a JSON object rather than being flattened to a string.
//
// This is why errorResponse takes `any` for its message. A client can highlight
// the individual inputs that failed, which it couldn't do with prose.
func TestFailedValidationResponse(t *testing.T) {
	app := newErrorTestApp(nil)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/movies", nil)

	app.failedValidationResponse(rr, r, map[string]string{
		"title": "must be provided",
		"year":  "must be greater than 1888",
	})

	rs := rr.Result()
	defer func() { _ = rs.Body.Close() }()

	assert.Equal(t, rs.StatusCode, http.StatusUnprocessableEntity)

	var env struct {
		Error map[string]string `json:"error"`
	}

	body, err := io.ReadAll(rs.Body)
	assert.NilError(t, err)
	assert.NilError(t, json.Unmarshal(body, &env))

	assert.DeepEqual(t, env.Error, map[string]string{
		"title": "must be provided",
		"year":  "must be greater than 1888",
	})
}

// TestErrorResponseFallsBackWhenEncodingFails covers errorResponse's own error
// path — what happens when we can't even write the error response.
//
// A channel can't be marshalled to JSON, so writeJSON fails. There's nothing
// useful left to send at that point, so the contract is: log it, and send a
// bare 500 rather than a half-written body.
func TestErrorResponseFallsBackWhenEncodingFails(t *testing.T) {
	var logBuf bytes.Buffer

	app := newErrorTestApp(&logBuf)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/movies", nil)

	// json.Marshal cannot encode a channel.
	app.errorResponse(rr, r, http.StatusBadRequest, make(chan int))

	assert.Equal(t, rr.Result().StatusCode, http.StatusInternalServerError)

	// The real cause has to reach the log, or this failure would be invisible.
	assert.StringContains(t, logBuf.String(), "json")
}

// assertableError is a minimal error type, so the test above doesn't need to
// import errors just to build one.
type assertableError string

func (e assertableError) Error() string { return string(e) }
