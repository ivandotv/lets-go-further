package main

import (
	"net/http"
	"testing"
	"time"

	"greenlight/internal/assert"
	"greenlight/internal/data"
)

func TestCreateAuthenticationToken(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	// Create a user we know the password for.
	user := &data.User{Name: "Alice", Email: "alice@example.com", Activated: true}
	assert.NilError(t, user.Password.Set("pa55word1234"))
	assert.NilError(t, app.models.Users.Insert(user))

	t.Run("valid credentials return a token", func(t *testing.T) {
		code, _, body := ts.post(t, "/v1/tokens/authentication", "", map[string]any{
			"email":    "alice@example.com",
			"password": "pa55word1234",
		})

		assert.Equal(t, code, http.StatusCreated)

		var got struct {
			AuthenticationToken struct {
				Token  string `json:"token"`
				Expiry string `json:"expiry"`
			} `json:"authentication_token"`
		}
		unmarshal(t, body, &got)

		assert.Equal(t, len(got.AuthenticationToken.Token), 26)

		// The token must actually work.
		code, _, _ = ts.get(t, "/v1/healthcheck", got.AuthenticationToken.Token)
		assert.Equal(t, code, http.StatusOK)
	})

	// ── The two failure modes must be INDISTINGUISHABLE ──────────────────────
	//
	// A wrong password and an unregistered email must produce byte-identical
	// responses. If they differed, an attacker could enumerate which email
	// addresses have accounts here — a real and commonly-exploited leak.
	t.Run("wrong password and unknown email are indistinguishable", func(t *testing.T) {
		codeWrongPassword, _, bodyWrongPassword := ts.post(t, "/v1/tokens/authentication", "", map[string]any{
			"email":    "alice@example.com",
			"password": "wrong-password-here",
		})

		codeUnknownEmail, _, bodyUnknownEmail := ts.post(t, "/v1/tokens/authentication", "", map[string]any{
			"email":    "nobody@example.com",
			"password": "pa55word1234",
		})

		assert.Equal(t, codeWrongPassword, http.StatusUnauthorized)
		assert.Equal(t, codeUnknownEmail, http.StatusUnauthorized)
		assert.Equal(t, bodyWrongPassword, bodyUnknownEmail)
	})

	t.Run("validation failures return 422", func(t *testing.T) {
		tests := []struct {
			name string
			body map[string]any
		}{
			{name: "missing email", body: map[string]any{"password": "pa55word1234"}},
			{name: "malformed email", body: map[string]any{"email": "nope", "password": "pa55word1234"}},
			{name: "missing password", body: map[string]any{"email": "alice@example.com"}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				code, _, _ := ts.post(t, "/v1/tokens/authentication", "", tt.body)
				assert.Equal(t, code, http.StatusUnprocessableEntity)
			})
		}
	})
}

// TestAuthenticationAndPermissions is the access-control matrix: it checks
// every combination of who you are against what you're allowed to do.
//
// This is the single most valuable test in the suite. Authorization bugs are
// both the easiest to introduce (one wrong wrapper in routes.go) and the most
// damaging, and a table like this makes the whole policy visible in one place.
func TestAuthenticationAndPermissions(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	readToken := createActivatedUser(t, app, "reader@example.com", data.PermissionMoviesRead)
	writeToken := createActivatedUser(t, app, "writer@example.com",
		data.PermissionMoviesRead, data.PermissionMoviesWrite)
	noPermsToken := createActivatedUser(t, app, "noperms@example.com")

	// An authenticated but UNACTIVATED user.
	inactive := &data.User{Name: "Inactive", Email: "inactive@example.com", Activated: false}
	assert.NilError(t, inactive.Password.Set("pa55word1234"))
	assert.NilError(t, app.models.Users.Insert(inactive))
	assert.NilError(t, app.models.Permissions.AddForUser(inactive.ID,
		data.PermissionMoviesRead, data.PermissionMoviesWrite))
	inactiveTok, err := app.models.Tokens.New(inactive.ID, 24*time.Hour, data.ScopeAuthentication)
	assert.NilError(t, err)

	movie := createTestMovie(t, app, "Test Movie", 2000)

	tests := []struct {
		name     string
		token    string
		method   string
		urlPath  string
		body     any
		wantCode int
	}{
		// ── Anonymous ────────────────────────────────────────────────────────
		{
			name: "anonymous cannot list movies", token: "",
			method: http.MethodGet, urlPath: "/v1/movies",
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "anonymous cannot create movies", token: "",
			method: http.MethodPost, urlPath: "/v1/movies",
			body:     map[string]any{"title": "X", "year": 2000, "runtime": "100 mins", "genres": []string{"drama"}},
			wantCode: http.StatusUnauthorized,
		},
		{
			// The healthcheck is deliberately public.
			name: "anonymous can call healthcheck", token: "",
			method: http.MethodGet, urlPath: "/v1/healthcheck",
			wantCode: http.StatusOK,
		},

		// ── Bad tokens ───────────────────────────────────────────────────────
		{
			name: "invalid token is rejected", token: "AAAAAAAAAAAAAAAAAAAAAAAAAA",
			method: http.MethodGet, urlPath: "/v1/movies",
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "malformed token is rejected", token: "too-short",
			method: http.MethodGet, urlPath: "/v1/movies",
			wantCode: http.StatusUnauthorized,
		},

		// ── Unactivated ──────────────────────────────────────────────────────
		{
			// 403, not 401: the credentials were fine, the account just isn't
			// activated. Retrying with a different token wouldn't help.
			name: "unactivated user is forbidden", token: inactiveTok.Plaintext,
			method: http.MethodGet, urlPath: "/v1/movies",
			wantCode: http.StatusForbidden,
		},

		// ── Activated, no permissions ────────────────────────────────────────
		{
			name: "no permissions cannot read", token: noPermsToken,
			method: http.MethodGet, urlPath: "/v1/movies",
			wantCode: http.StatusForbidden,
		},

		// ── movies:read only ─────────────────────────────────────────────────
		{
			name: "read permission can list", token: readToken,
			method: http.MethodGet, urlPath: "/v1/movies",
			wantCode: http.StatusOK,
		},
		{
			name: "read permission can show", token: readToken,
			method: http.MethodGet, urlPath: "/v1/movies/1",
			wantCode: http.StatusOK,
		},
		{
			// The key negative case: read does NOT imply write.
			name: "read permission cannot create", token: readToken,
			method: http.MethodPost, urlPath: "/v1/movies",
			body:     map[string]any{"title": "X", "year": 2000, "runtime": "100 mins", "genres": []string{"drama"}},
			wantCode: http.StatusForbidden,
		},
		{
			name: "read permission cannot delete", token: readToken,
			method: http.MethodDelete, urlPath: "/v1/movies/1",
			wantCode: http.StatusForbidden,
		},

		// ── movies:write ─────────────────────────────────────────────────────
		{
			name: "write permission can create", token: writeToken,
			method: http.MethodPost, urlPath: "/v1/movies",
			body:     map[string]any{"title": "New", "year": 2000, "runtime": "100 mins", "genres": []string{"drama"}},
			wantCode: http.StatusCreated,
		},
	}

	_ = movie // the movie exists so /v1/movies/1 resolves

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, _ := ts.do(t, tt.method, tt.urlPath, tt.token, tt.body)
			assert.Equal(t, code, tt.wantCode)
		})
	}
}

// TestMalformedAuthorizationHeader checks the header parsing itself.
func TestMalformedAuthorizationHeader(t *testing.T) {
	app, _ := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	tests := []struct {
		name   string
		header string
	}{
		{name: "no scheme", header: "AAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "wrong scheme", header: "Basic AAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "lowercase scheme", header: "bearer AAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "too many parts", header: "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAA extra"},
		{name: "scheme only", header: "Bearer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/movies", nil)
			assert.NilError(t, err)
			req.Header.Set("Authorization", tt.header)

			rs, err := ts.Client().Do(req)
			assert.NilError(t, err)
			defer rs.Body.Close()

			assert.Equal(t, rs.StatusCode, http.StatusUnauthorized)

			// RFC 7235 requires a WWW-Authenticate header on a 401, telling
			// the client which scheme to retry with.
			assert.Equal(t, rs.Header.Get("WWW-Authenticate"), "Bearer")
		})
	}
}
