package main

import (
	"net/http"
	"strings"
	"testing"

	"greenlight/internal/assert"
	"greenlight/internal/data"
)

// TestRegisterUser walks the whole signup flow end to end, including the
// background email.
func TestRegisterUser(t *testing.T) {
	app, mailer := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	t.Run("successful registration", func(t *testing.T) {
		code, _, body := ts.post(t, "/v1/users", "", map[string]any{
			"name":     "Alice",
			"email":    "alice@example.com",
			"password": "pa55word1234",
		})

		// 202 Accepted, not 201 Created: the account exists but the welcome
		// email hasn't been sent yet.
		assert.Equal(t, code, http.StatusAccepted)

		var got struct {
			User struct {
				ID        int64  `json:"id"`
				Name      string `json:"name"`
				Email     string `json:"email"`
				Activated bool   `json:"activated"`
			} `json:"user"`
		}
		unmarshal(t, body, &got)

		assert.Equal(t, got.User.Name, "Alice")
		assert.Equal(t, got.User.Email, "alice@example.com")
		// New accounts must start deactivated.
		assert.Equal(t, got.User.Activated, false)

		// The password hash must never appear in a response. This is what the
		// `json:"-"` tag on the Password field buys us, and it's worth an
		// explicit test because a careless refactor could remove it.
		if len(body) > 0 {
			assert.Equal(t, containsAny(body, "password", "hash", "$2a$"), false)
		}

		// The welcome email is sent from a background goroutine, so wait for it.
		msgs := mailer.waitForEmail(t, 1)
		assert.Equal(t, msgs[0].Recipient, "alice@example.com")
		assert.Equal(t, msgs[0].Template, "user_welcome.tmpl")

		// The activation token must be in the email — and nowhere else.
		token, ok := msgs[0].Data["activationToken"].(string)
		if !ok || len(token) != 26 {
			t.Fatalf("expected a 26-character activation token in the email data; got %v", msgs[0].Data["activationToken"])
		}
	})

	t.Run("new users get movies:read but not movies:write", func(t *testing.T) {
		code, _, _ := ts.post(t, "/v1/users", "", map[string]any{
			"name":     "Bob",
			"email":    "bob@example.com",
			"password": "pa55word1234",
		})
		assert.Equal(t, code, http.StatusAccepted)

		user, err := app.models.Users.GetByEmail("bob@example.com")
		assert.NilError(t, err)

		permissions, err := app.models.Permissions.GetAllForUser(user.ID)
		assert.NilError(t, err)

		assert.Equal(t, permissions.Include(data.PermissionMoviesRead), true)
		assert.Equal(t, permissions.Include(data.PermissionMoviesWrite), false)
	})

	t.Run("duplicate email returns 422", func(t *testing.T) {
		body := map[string]any{
			"name":     "Carol",
			"email":    "carol@example.com",
			"password": "pa55word1234",
		}

		code, _, _ := ts.post(t, "/v1/users", "", body)
		assert.Equal(t, code, http.StatusAccepted)

		code, _, respBody := ts.post(t, "/v1/users", "", body)
		assert.Equal(t, code, http.StatusUnprocessableEntity)
		assert.StringContains(t, respBody, "already exists")
	})

	t.Run("validation failures", func(t *testing.T) {
		tests := []struct {
			name    string
			body    map[string]any
			wantKey string
		}{
			{
				name:    "missing name",
				body:    map[string]any{"email": "x@example.com", "password": "pa55word1234"},
				wantKey: "name",
			},
			{
				name:    "malformed email",
				body:    map[string]any{"name": "X", "email": "not-an-email", "password": "pa55word1234"},
				wantKey: "email",
			},
			{
				name:    "password too short",
				body:    map[string]any{"name": "X", "email": "x@example.com", "password": "short"},
				wantKey: "password",
			},
			{
				name:    "missing password",
				body:    map[string]any{"name": "X", "email": "x@example.com", "password": ""},
				wantKey: "password",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				code, _, body := ts.post(t, "/v1/users", "", tt.body)

				assert.Equal(t, code, http.StatusUnprocessableEntity)
				assert.StringContains(t, body, tt.wantKey)
			})
		}
	})
}

// TestActivateUser covers the second half of signup: exchanging the emailed
// token for an activated account.
func TestActivateUser(t *testing.T) {
	app, mailer := newTestApplication(t)
	ts := newTestServer(t, app.routes())

	// Register, then pull the token out of the mock mailer — exactly what a
	// real user does with their inbox.
	code, _, _ := ts.post(t, "/v1/users", "", map[string]any{
		"name":     "Alice",
		"email":    "alice@example.com",
		"password": "pa55word1234",
	})
	assert.Equal(t, code, http.StatusAccepted)

	msgs := mailer.waitForEmail(t, 1)
	token := msgs[0].Data["activationToken"].(string)

	t.Run("invalid token is rejected", func(t *testing.T) {
		code, _, body := ts.put(t, "/v1/users/activated", "",
			map[string]any{"token": "AAAAAAAAAAAAAAAAAAAAAAAAAA"})

		assert.Equal(t, code, http.StatusUnprocessableEntity)
		assert.StringContains(t, body, "invalid or expired activation token")
	})

	t.Run("malformed token is rejected", func(t *testing.T) {
		code, _, body := ts.put(t, "/v1/users/activated", "", map[string]any{"token": "tooshort"})

		assert.Equal(t, code, http.StatusUnprocessableEntity)
		assert.StringContains(t, body, "token")
	})

	t.Run("valid token activates the account", func(t *testing.T) {
		code, _, body := ts.put(t, "/v1/users/activated", "", map[string]any{"token": token})

		assert.Equal(t, code, http.StatusOK)

		var got struct {
			User struct {
				Activated bool `json:"activated"`
			} `json:"user"`
		}
		unmarshal(t, body, &got)
		assert.Equal(t, got.User.Activated, true)
	})

	t.Run("token cannot be reused", func(t *testing.T) {
		// This subtest MUST run after the one above. Activation deletes all of
		// the user's activation tokens, so replaying the same token — from a
		// forwarded email, a log file, a shoulder-surf — must now fail.
		code, _, body := ts.put(t, "/v1/users/activated", "", map[string]any{"token": token})

		assert.Equal(t, code, http.StatusUnprocessableEntity)
		assert.StringContains(t, body, "invalid or expired activation token")
	})
}

// containsAny reports whether s contains any of the substrings. Used above to
// assert that sensitive material is absent from a response body.
func containsAny(s string, substrings ...string) bool {
	for _, sub := range substrings {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}
