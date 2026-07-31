package config

import (
	"os"
	"path/filepath"
	"testing"
)

// docs/stability-policy.md defines deprecation as announce -> coexist -> remove.
// Coexistence worked, but the announcement lived only in release notes, so a
// config using a legacy spelling validated clean and its owner never learned the
// spelling was going away (#324). Load now reports what it rewrote.

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "k8shark.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_ReportsLegacyKeys(t *testing.T) {
	path := writeConfig(t, `
duration: 1m
auto_discover: true
ui:
  api_port: "8081"
resources:
  - group: ""
    version: v1
    resource: pods
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The rewrite itself must still work — this is a warning, not a rejection.
	if !cfg.AutoDiscover {
		t.Error("autoDiscover not applied from the legacy spelling")
	}
	if cfg.UI.APIPort != "8081" {
		t.Errorf("ui.apiPort = %q, want 8081 from the legacy spelling", cfg.UI.APIPort)
	}

	if len(cfg.Deprecations) != 2 {
		t.Fatalf("Deprecations = %+v, want 2 entries", cfg.Deprecations)
	}
	// Sorted, so the output is stable across runs despite map iteration order.
	if cfg.Deprecations[0].Key != "auto_discover" || cfg.Deprecations[0].Replacement != "autoDiscover" {
		t.Errorf("[0] = %+v", cfg.Deprecations[0])
	}
	if cfg.Deprecations[1].Key != "ui.api_port" || cfg.Deprecations[1].Replacement != "ui.apiPort" {
		t.Errorf("[1] = %+v", cfg.Deprecations[1])
	}
	for _, d := range cfg.Deprecations {
		if d.Ignored {
			t.Errorf("%s marked Ignored, but no canonical key was set", d.Key)
		}
	}
}

// Deprecations is assigned before mapstructure.Decode runs over the same map.
// The `mapstructure:"-"` tag has to keep the decode from clobbering it.
func TestLoad_DeprecationsSurviveDecode(t *testing.T) {
	path := writeConfig(t, `
duration: 1m
auto_discover: true
resources:
  - group: ""
    version: v1
    resource: pods
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Deprecations) == 0 {
		t.Fatal("Deprecations was cleared by the mapstructure decode")
	}
}

// When both spellings are present the canonical one wins and the legacy value is
// discarded — a setting in the file that has no effect, which is worth saying.
func TestLoad_BothSpellingsMarksLegacyIgnored(t *testing.T) {
	path := writeConfig(t, `
duration: 1m
ui:
  api_port: "9999"
  apiPort: "8081"
resources:
  - group: ""
    version: v1
    resource: pods
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UI.APIPort != "8081" {
		t.Errorf("ui.apiPort = %q, want the canonical 8081 to win", cfg.UI.APIPort)
	}
	if len(cfg.Deprecations) != 1 {
		t.Fatalf("Deprecations = %+v, want 1", cfg.Deprecations)
	}
	if !cfg.Deprecations[0].Ignored {
		t.Error("legacy key not marked Ignored even though the canonical key was also set")
	}
}

// A config using only canonical spellings must report nothing. #240 removed
// spurious validate warnings; this warning must never fire on a current config.
func TestLoad_CanonicalConfigReportsNothing(t *testing.T) {
	path := writeConfig(t, `
duration: 1m
autoDiscover: true
ui:
  apiPort: "8081"
resources:
  - group: ""
    version: v1
    resource: pods
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Deprecations) != 0 {
		t.Errorf("Deprecations = %+v, want none for an all-canonical config", cfg.Deprecations)
	}
}

// The shipped examples must not trip the warning, or every user sees it on day
// one and learns to ignore it.
func TestLoad_ShippedExamplesHaveNoDeprecations(t *testing.T) {
	for _, path := range []string{
		"../../examples/k8shark.yaml",
		"../../docs/demo/demo-capture.yaml",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Skipf("%s not present: %v", path, err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%s): %v", path, err)
		}
		if len(cfg.Deprecations) != 0 {
			t.Errorf("%s reports deprecations %+v; a shipped example must use canonical spellings", path, cfg.Deprecations)
		}
	}
}
