// Package envflag lets command-line flags fall back to environment variables.
//
// ── WHY THIS EXISTS ──────────────────────────────────────────────────────────
//
// The book configures everything with flags, which is ideal locally: `-help`
// lists every setting with its type and default, and you can override one of
// them for a single run without exporting anything.
//
// Flags alone stop being ideal in two places:
//
//  1. Command-line arguments are world-readable in `ps aux`. Passing
//     -smtp-password on the command line shows the credential to every user on
//     the host.
//
//  2. Container platforms inject configuration as environment variables.
//     Docker's `env_file:`, Kubernetes Secrets and ConfigMaps all set env vars;
//     none of them set argv. Without env support you end up writing an
//     entrypoint script that assembles a command line out of env vars, which is
//     the worst of both worlds.
//
// So rather than replacing flags, this package layers the environment
// underneath them. Precedence runs:
//
//	command-line flag  >  environment variable  >  the flag's own default
//
// which is the same order docker, kubectl, git and psql all use, so there is no
// project-specific scheme for anyone to learn.
//
// Nothing is wired up per flag. The environment variable name is derived
// mechanically from the flag name, so a flag added later gets an environment
// variable automatically, with no change here and no change at the call site.
package envflag

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// Name returns the environment variable that backs the given flag.
//
// The mapping is upper-case with dashes turned into underscores, so with a
// "GREENLIGHT_" prefix the -db-max-open-conns flag reads GREENLIGHT_DB_MAX_OPEN_CONNS.
func Name(prefix, flagName string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Apply sets any flag that was NOT given on the command line from its
// environment variable, when that variable is present.
//
// Call it after fs.Parse. Flags named in skip are left alone entirely — see
// Usage for the reasoning.
//
// Parsing is delegated back to the flag package: fs.Set runs exactly the same
// Value.Set that the command line would, so every type keeps its usual parsing
// and validation, and a flag.Func callback still runs. That is what keeps this
// helper type-agnostic despite never mentioning a type.
//
// An unset variable and an empty one are deliberately different: FOO= means
// "explicitly empty" and is applied as such, matching how the shell and every
// container runtime treat it.
func Apply(fs *flag.FlagSet, prefix string, skip ...string) error {
	// fs.Visit walks only the flags actually PRESENT on the command line
	// (unlike fs.VisitAll, which walks every defined flag). That distinction is
	// the whole mechanism: it's what lets an explicit flag outrank the
	// environment without tracking anything ourselves.
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	for _, name := range skip {
		given[name] = true
	}

	// VisitAll has no way to stop early or return an error, so the first
	// failure is captured here and the remaining flags are skipped.
	var err error

	fs.VisitAll(func(f *flag.Flag) {
		if err != nil || given[f.Name] {
			return
		}

		key := Name(prefix, f.Name)

		val, ok := os.LookupEnv(key)
		if !ok {
			return
		}

		if setErr := fs.Set(f.Name, val); setErr != nil {
			// The wrapped error quotes the offending value, which is fine here:
			// Set only fails for typed flags (ints, bools, durations), and
			// secrets are always plain strings, whose Set cannot fail.
			err = fmt.Errorf("invalid %s: %w", key, setErr)
		}
	})

	return err
}

// Usage returns a replacement for fs.Usage that documents each flag's
// environment variable next to its default.
//
// This is the reason to keep flags rather than move to environment variables
// wholesale: `-help` stays the single, always-current list of every setting.
// A hand-maintained list in the README drifts the first time someone adds a
// flag and forgets; this cannot.
//
// Flags named in skip are printed without an [env: ...] line, because Apply
// ignores them too. -version is the motivating case: GREENLIGHT_VERSION is a
// plausible variable for a deployment to set for unrelated reasons, and mapping
// it onto a bool flag would fail to parse and take the server down at boot.
func Usage(fs *flag.FlagSet, prefix string, skip ...string) func() {
	skipped := make(map[string]bool, len(skip))
	for _, name := range skip {
		skipped[name] = true
	}

	return func() {
		// Writes to the usage output are unchecked, matching the flag package's
		// own PrintDefaults: there is nothing useful to do if printing a help
		// message fails, and the process is on its way out regardless.
		out := fs.Output()

		_, _ = fmt.Fprintf(out, "Usage of %s:\n", fs.Name())

		fs.VisitAll(func(f *flag.Flag) {
			_, _ = fmt.Fprintf(out, "  -%s\n", f.Name)
			_, _ = fmt.Fprintf(out, "    \t%s", f.Usage)

			if f.DefValue != "" {
				_, _ = fmt.Fprintf(out, " (default %s)", f.DefValue)
			}

			_, _ = fmt.Fprintln(out)

			if !skipped[f.Name] {
				_, _ = fmt.Fprintf(out, "    \t[env: %s]\n", Name(prefix, f.Name))
			}
		})
	}
}
