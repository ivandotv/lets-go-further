package envflag

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"greenlight/internal/assert"
)

// newFlagSet builds a FlagSet covering every flag TYPE the two commands use, so
// the tests exercise the delegation to Value.Set rather than just strings.
//
// Output goes to io.Discard by default: ContinueOnError still prints the usage
// message on a parse failure, and the tests that expect one shouldn't spray it
// over the test log.
func newFlagSet() (*flag.FlagSet, *struct {
	port     int
	dsn      string
	rps      float64
	enabled  bool
	idleTime time.Duration
	origins  []string
},
) {
	var vals struct {
		port     int
		dsn      string
		rps      float64
		enabled  bool
		idleTime time.Duration
		origins  []string
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.IntVar(&vals.port, "port", 4000, "port")
	fs.StringVar(&vals.dsn, "db-dsn", "greenlight.db", "dsn")
	fs.Float64Var(&vals.rps, "limiter-rps", 2, "rps")
	fs.BoolVar(&vals.enabled, "limiter-enabled", true, "enabled")
	fs.DurationVar(&vals.idleTime, "db-max-idle-time", 15*time.Minute, "idle time")
	fs.Func("cors-trusted-origins", "origins", func(val string) error {
		vals.origins = strings.Fields(val)
		return nil
	})

	return fs, &vals
}

func TestName(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		flagName string
		want     string
	}{
		{"simple", "GREENLIGHT_", "port", "GREENLIGHT_PORT"},
		{"dashes become underscores", "GREENLIGHT_", "db-max-open-conns", "GREENLIGHT_DB_MAX_OPEN_CONNS"},
		{"empty prefix", "", "port", "PORT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Name(tt.prefix, tt.flagName), tt.want)
		})
	}
}

// TestApplyUsesEnvWhenFlagAbsent is the base case: no arguments at all, so
// every value comes from the environment.
//
// Each flag type is covered because Apply never parses anything itself — it
// hands the string back to the flag package — and this is what proves that.
func TestApplyUsesEnvWhenFlagAbsent(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "9999")
	t.Setenv("GREENLIGHT_DB_DSN", "/tmp/from-env.db")
	t.Setenv("GREENLIGHT_LIMITER_RPS", "7.5")
	t.Setenv("GREENLIGHT_LIMITER_ENABLED", "false")
	t.Setenv("GREENLIGHT_DB_MAX_IDLE_TIME", "30s")

	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse(nil))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.Equal(t, vals.port, 9999)
	assert.Equal(t, vals.dsn, "/tmp/from-env.db")
	assert.Equal(t, vals.rps, 7.5)
	assert.Equal(t, vals.enabled, false)
	assert.Equal(t, vals.idleTime, 30*time.Second)
}

// TestApplyFlagBeatsEnv pins the precedence rule that the whole design rests
// on. If this ever inverted, a deployment's environment would silently override
// an explicitly-passed flag — the least debuggable failure mode available.
func TestApplyFlagBeatsEnv(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "9999")
	t.Setenv("GREENLIGHT_DB_DSN", "/tmp/from-env.db")

	fs, vals := newFlagSet()

	// -port given explicitly, -db-dsn not.
	assert.NilError(t, fs.Parse([]string{"-port=4001"}))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.Equal(t, vals.port, 4001)
	assert.Equal(t, vals.dsn, "/tmp/from-env.db")
}

// TestApplyKeepsDefaultWhenNeitherGiven covers the third rung of the ladder.
func TestApplyKeepsDefaultWhenNeitherGiven(t *testing.T) {
	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse(nil))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.Equal(t, vals.port, 4000)
	assert.Equal(t, vals.dsn, "greenlight.db")
	assert.Equal(t, vals.enabled, true)
}

// TestApplyMatchesExplicitFlagEvenWhenItEqualsTheDefault guards the reason
// Apply uses fs.Visit rather than comparing against DefValue.
//
// `-port=4000` is indistinguishable from an unset -port if you only look at the
// resulting value, and a DefValue comparison would wrongly let the environment
// win here.
func TestApplyMatchesExplicitFlagEvenWhenItEqualsTheDefault(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "9999")

	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse([]string{"-port=4000"}))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.Equal(t, vals.port, 4000)
}

// TestApplyRunsFuncFlags checks that flag.Func callbacks fire for environment
// values too — without this, GREENLIGHT_CORS_TRUSTED_ORIGINS would be accepted
// and then silently ignored.
func TestApplyRunsFuncFlags(t *testing.T) {
	t.Setenv("GREENLIGHT_CORS_TRUSTED_ORIGINS", "https://a.example.com https://b.example.com")

	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse(nil))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.DeepEqual(t, vals.origins, []string{"https://a.example.com", "https://b.example.com"})
}

// TestApplyEmptyEnvIsApplied documents the unset/empty distinction.
//
// FOO= means "explicitly empty" to the shell and to every container runtime, so
// it's applied rather than skipped. It matters for -smtp-host, where empty is a
// meaningful value that selects the logging mailer.
func TestApplyEmptyEnvIsApplied(t *testing.T) {
	t.Setenv("GREENLIGHT_DB_DSN", "")

	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse(nil))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.Equal(t, vals.dsn, "")
}

// TestApplyRejectsUnparseableEnv checks that a bad value fails loudly at boot
// rather than silently falling back to the default.
func TestApplyRejectsUnparseableEnv(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "not-a-number")

	fs, _ := newFlagSet()

	assert.NilError(t, fs.Parse(nil))

	err := Apply(fs, "GREENLIGHT_")
	if err == nil {
		t.Fatal("got nil error for an unparseable GREENLIGHT_PORT; want a failure")
	}

	// The message has to name the variable — "invalid syntax" alone would send
	// you hunting through the flags for a problem that's in the environment.
	assert.StringContains(t, err.Error(), "GREENLIGHT_PORT")
}

// TestApplySkipsNamedFlags covers the -version exclusion.
func TestApplySkipsNamedFlags(t *testing.T) {
	t.Setenv("GREENLIGHT_PORT", "9999")

	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse(nil))
	assert.NilError(t, Apply(fs, "GREENLIGHT_", "port"))

	assert.Equal(t, vals.port, 4000)
}

// TestApplyIgnoresOtherPrefixes makes sure an unprefixed variable of the same
// name can't leak in. A bare PORT is set by all sorts of platforms.
func TestApplyIgnoresOtherPrefixes(t *testing.T) {
	t.Setenv("PORT", "9999")

	fs, vals := newFlagSet()

	assert.NilError(t, fs.Parse(nil))
	assert.NilError(t, Apply(fs, "GREENLIGHT_"))

	assert.Equal(t, vals.port, 4000)
}

// TestUsageNamesEnvVars is what keeps `-help` the single source of truth for
// configuration. If the [env: ...] lines disappeared, the only complete list of
// settings would be a hand-maintained one in the README.
func TestUsageNamesEnvVars(t *testing.T) {
	var buf bytes.Buffer

	fs, _ := newFlagSet()
	fs.SetOutput(&buf)

	Usage(fs, "GREENLIGHT_", "port")()

	out := buf.String()

	assert.StringContains(t, out, "-db-dsn")
	assert.StringContains(t, out, "[env: GREENLIGHT_DB_DSN]")
	assert.StringContains(t, out, "[env: GREENLIGHT_CORS_TRUSTED_ORIGINS]")

	// Defaults still appear, as they do in the flag package's own output.
	assert.StringContains(t, out, "(default greenlight.db)")

	// Skipped flags are listed, but without an environment variable — claiming
	// GREENLIGHT_PORT works when Apply ignores it would be worse than silence.
	assert.StringContains(t, out, "-port")

	if strings.Contains(out, "[env: GREENLIGHT_PORT]") {
		t.Error("usage advertised GREENLIGHT_PORT for a flag that Apply skips")
	}
}
