package cmd

import (
	"fmt"
	"io"

	"github.com/phenixblue/k8shark/internal/config"
)

// warnDeprecatedConfigKeys prints one line per legacy config key the load
// rewrote, naming the replacement.
//
// docs/stability-policy.md defines deprecation as announce -> coexist -> remove.
// The coexistence was implemented but the announcement only ever existed in
// release notes, so a config using a legacy spelling validated completely clean
// and its owner had no way to learn the spelling was on its way out — until a
// later minor removed it (#324).
//
// Deliberately not a validation failure: warnings go to stderr so `-o json`
// stdout stays machine-readable, and the exit code is untouched. Per the
// exit-code contract only `diagnose --fail-on` and `diff` may exit 1, and a
// deprecated-but-working key is not a failure.
//
// It fires only when a legacy key was actually present, so a config using
// current spellings stays silent — #240 removed spurious validate warnings and
// this must not reintroduce the habit of ignoring them.
func warnDeprecatedConfigKeys(w io.Writer, cfg *config.Config) {
	if cfg == nil || len(cfg.Deprecations) == 0 {
		return
	}
	for _, d := range cfg.Deprecations {
		if d.Ignored {
			fmt.Fprintf(w, "⚠ config: %q is deprecated and was ignored because %q is also set; remove %q\n",
				d.Key, d.Replacement, d.Key)
			continue
		}
		fmt.Fprintf(w, "⚠ config: %q is deprecated; use %q\n", d.Key, d.Replacement)
	}
	fmt.Fprintf(w, "  the legacy spellings still work, but will be removed in a future minor release — see docs/stability-policy.md#deprecation-policy\n")
}
