package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/config"
)

func TestWarnDeprecatedConfigKeys(t *testing.T) {
	t.Run("silent for a current config", func(t *testing.T) {
		var buf bytes.Buffer
		warnDeprecatedConfigKeys(&buf, &config.Config{})
		if buf.Len() != 0 {
			// #240 removed spurious validate warnings; reintroducing one that
			// fires on a clean config would teach users to ignore the channel.
			t.Errorf("wrote %q for a config with no deprecations", buf.String())
		}
	})

	t.Run("nil config is safe", func(t *testing.T) {
		var buf bytes.Buffer
		warnDeprecatedConfigKeys(&buf, nil)
		if buf.Len() != 0 {
			t.Errorf("wrote %q for a nil config", buf.String())
		}
	})

	t.Run("names the replacement", func(t *testing.T) {
		var buf bytes.Buffer
		warnDeprecatedConfigKeys(&buf, &config.Config{Deprecations: []config.Deprecation{
			{Key: "auto_discover", Replacement: "autoDiscover"},
			{Key: "ui.api_port", Replacement: "ui.apiPort"},
		}})
		out := buf.String()
		for _, want := range []string{"auto_discover", "autoDiscover", "ui.api_port", "ui.apiPort"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		// The announcement is the point — a user has to learn it's going away.
		if !strings.Contains(out, "removed in a future minor release") {
			t.Errorf("output doesn't say the spelling will be removed:\n%s", out)
		}
		if !strings.Contains(out, "stability-policy") {
			t.Errorf("output doesn't point at the policy:\n%s", out)
		}
		// One summary line, not one per key.
		if n := strings.Count(out, "removed in a future minor release"); n != 1 {
			t.Errorf("summary line repeated %d times, want 1:\n%s", n, out)
		}
	})

	t.Run("says so when the legacy value was discarded", func(t *testing.T) {
		var buf bytes.Buffer
		warnDeprecatedConfigKeys(&buf, &config.Config{Deprecations: []config.Deprecation{
			{Key: "ui.api_port", Replacement: "ui.apiPort", Ignored: true},
		}})
		out := buf.String()
		if !strings.Contains(out, "ignored") {
			t.Errorf("output doesn't say the legacy value was ignored:\n%s", out)
		}
	})
}
