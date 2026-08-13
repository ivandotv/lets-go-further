package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"greenlight/internal/assert"
	"greenlight/internal/mailer"
)

// TestNewMailerFallsBackToLogging covers the "no SMTP configured" branch.
//
// This is the deviation from the book that lets a fresh clone register a user
// and read the activation token out of the logs without a Mailtrap account. If
// it regressed to returning a real *mailer.Mailer with an empty host, every
// registration would spend 5 seconds timing out against nothing — and since the
// send happens in a background goroutine, the request would still return 202
// and nobody would notice until they went looking for the email.
func TestNewMailerFallsBackToLogging(t *testing.T) {
	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, nil))

	var cfg config
	cfg.smtp.host = "" // the default

	m := newMailer(cfg, logger)

	if _, ok := m.(logMailer); !ok {
		t.Fatalf("got %T with no -smtp-host set; want logMailer", m)
	}

	// It says so, so the behaviour isn't a surprise.
	assert.StringContains(t, buf.String(), "emails will be logged instead of sent")
}

// TestNewMailerUsesSMTPWhenConfigured covers the other branch.
func TestNewMailerUsesSMTPWhenConfigured(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	var cfg config
	cfg.smtp.host = "smtp.example.com"
	cfg.smtp.port = 25
	cfg.smtp.sender = "Greenlight <no-reply@example.com>"

	m := newMailer(cfg, logger)

	if _, ok := m.(*mailer.Mailer); !ok {
		t.Fatalf("got %T with an -smtp-host set; want *mailer.Mailer", m)
	}
}

// TestLogMailerSendLogsTheToken is the assertion that makes the fallback
// actually useful.
//
// The activation token lives in the template data, so logging the recipient and
// template name alone would be worthless — you'd know an email "was sent" but
// still have no way to activate the account.
func TestLogMailerSendLogsTheToken(t *testing.T) {
	var buf bytes.Buffer

	m := logMailer{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	err := m.Send("alice@example.com", "user_welcome.tmpl", map[string]any{
		"userID":          int64(1),
		"activationToken": "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"name":            "Alice",
	})
	assert.NilError(t, err)

	logged := buf.String()

	assert.StringContains(t, logged, "alice@example.com")
	assert.StringContains(t, logged, "user_welcome.tmpl")
	assert.StringContains(t, logged, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

// TestVCSRevision checks the version string the healthcheck reports.
//
// Since Go 1.18 the toolchain embeds VCS information automatically, so this
// returns a real short commit hash when built inside a repository — and the
// point of the test is that it always returns SOMETHING usable, never an empty
// string, because the healthcheck endpoint publishes it.
func TestVCSRevision(t *testing.T) {
	got := vcsRevision()

	if got == "" {
		t.Fatal("vcsRevision returned an empty string; the healthcheck would report no version at all")
	}

	// Either the fallback, or a short hash (optionally marked dirty).
	if got == "unavailable" {
		t.Log("no VCS info in this build; got the fallback, which is the documented behaviour")
		return
	}

	// Short hash is truncated to 12 characters, plus an optional suffix.
	hash, _, _ := strings.Cut(got, "-")
	if len(hash) > 12 {
		t.Errorf("got revision %q; the hash part should be truncated to 12 characters", got)
	}

	for _, r := range hash {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("got revision %q; the hash part should be hexadecimal", got)
			break
		}
	}
}

// TestHealthcheckReportsVersionAndEnv checks the endpoint's full payload.
//
// The existing TestHealthcheck asserts the status is available; this pins the
// other two fields, which are what you actually look at when you're trying to
// work out which build is running in an environment.
func TestHealthcheckReportsVersionAndEnv(t *testing.T) {
	app := &application{}
	app.config.env = "testing"

	rr := httptest.NewRecorder()
	app.healthcheckHandler(rr, httptest.NewRequest(http.MethodGet, "/v1/healthcheck", nil))

	rs := rr.Result()
	defer func() { _ = rs.Body.Close() }()

	assert.Equal(t, rs.StatusCode, http.StatusOK)

	var env struct {
		Status     string `json:"status"`
		SystemInfo struct {
			Environment string `json:"environment"`
			Version     string `json:"version"`
		} `json:"system_info"`
	}

	if err := json.NewDecoder(rs.Body).Decode(&env); err != nil {
		t.Fatalf("decoding healthcheck response: %v", err)
	}

	assert.Equal(t, env.Status, "available")
	assert.Equal(t, env.SystemInfo.Environment, "testing")

	if env.SystemInfo.Version == "" {
		t.Error("healthcheck reported an empty version")
	}
}
