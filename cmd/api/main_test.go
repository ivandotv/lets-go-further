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

// TestParseConfigDefaults checks that the zero-argument case still produces a
// runnable configuration — the property that lets `go run ./cmd/api` work in a
// fresh clone with no setup.
func TestParseConfigDefaults(t *testing.T) {
	cfg, showVersion, err := parseConfig(nil)
	assert.NilError(t, err)

	assert.Equal(t, showVersion, false)
	assert.Equal(t, cfg.port, 4000)
	assert.Equal(t, cfg.env, "development")
	assert.Equal(t, cfg.db.dsn, "greenlight.db")
	assert.Equal(t, cfg.limiter.enabled, true)
}

// TestParseConfigReadsEnvironment is the end-to-end version of the envflag
// tests: it proves the wiring in parseConfig actually reaches the real flags,
// including the SMTP password that motivated the whole change.
func TestParseConfigReadsEnvironment(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "8080")
	t.Setenv("GREENLIGHT_ENV", "production")
	t.Setenv("GREENLIGHT_SMTP_PASSWORD", "s3cret")
	t.Setenv("GREENLIGHT_CORS_TRUSTED_ORIGINS", "https://a.example.com https://b.example.com")

	cfg, _, err := parseConfig(nil)
	assert.NilError(t, err)

	assert.Equal(t, cfg.port, 8080)
	assert.Equal(t, cfg.env, "production")
	assert.Equal(t, cfg.smtp.password, "s3cret")
	assert.DeepEqual(t, cfg.cors.trustedOrigins, []string{"https://a.example.com", "https://b.example.com"})
}

// TestParseConfigFlagBeatsEnvironment pins the precedence rule at the level
// people actually rely on it — overriding one setting for a single local run
// while .env supplies the rest.
func TestParseConfigFlagBeatsEnvironment(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "8080")
	t.Setenv("GREENLIGHT_DB_DSN", "/tmp/from-env.db")

	cfg, _, err := parseConfig([]string{"-port=4001"})
	assert.NilError(t, err)

	assert.Equal(t, cfg.port, 4001)
	assert.Equal(t, cfg.db.dsn, "/tmp/from-env.db")
}

// TestParseConfigIgnoresVersionEnv covers the deliberate exclusion.
//
// GREENLIGHT_VERSION is a plausible thing for a deployment to set for its own
// reasons; mapping it onto the -version bool would fail to parse and stop the
// server booting. It must be inert.
func TestParseConfigIgnoresVersionEnv(t *testing.T) {
	t.Setenv("GREENLIGHT_VERSION", "1.2.3")

	_, showVersion, err := parseConfig(nil)
	assert.NilError(t, err)
	assert.Equal(t, showVersion, false)
}

// TestParseConfigVersionFlag checks the flag itself still works.
func TestParseConfigVersionFlag(t *testing.T) {
	_, showVersion, err := parseConfig([]string{"-version"})
	assert.NilError(t, err)
	assert.Equal(t, showVersion, true)
}

// TestRunHelpExitsCleanly checks that `api -help` is treated as success.
//
// With ContinueOnError, fs.Parse returns flag.ErrHelp for -help. If run()
// passed that straight up, main would print "fatal: flag: help requested" and
// exit 1 — for what is a completely normal thing to type.
func TestRunHelpExitsCleanly(t *testing.T) {
	assert.NilError(t, run([]string{"-help"}))
}

// TestRunRejectsBadEnvironment checks that a malformed environment value stops
// the server at boot with a message naming the variable, rather than silently
// running on the default.
func TestRunRejectsBadEnvironment(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "not-a-number")

	err := run(nil)
	if err == nil {
		t.Fatal("got nil error with an unparseable GREENLIGHT_PORT; want a failure")
	}

	assert.StringContains(t, err.Error(), "GREENLIGHT_PORT")
}

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
