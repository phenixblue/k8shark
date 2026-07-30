package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// writeConfigFile writes contents to a temp *.yaml file and returns its path.
func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "k8shark.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing config file: %v", err)
	}
	return path
}

func TestLoad_BasicFile(t *testing.T) {
	path := writeConfigFile(t, `
duration: 5m
output: ./capture.kshrk
resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [default]
    interval: 30s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DurationRaw != "5m" {
		t.Errorf("DurationRaw = %q, want 5m", cfg.DurationRaw)
	}
	if cfg.Output != "./capture.kshrk" {
		t.Errorf("Output = %q, want ./capture.kshrk", cfg.Output)
	}
	if len(cfg.Resources) != 1 || cfg.Resources[0].Resource != "pods" {
		t.Fatalf("unexpected Resources: %+v", cfg.Resources)
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.DurationRaw != "" || cfg.Output != "" || len(cfg.Resources) != 0 {
		t.Errorf("expected every file-sourced field to stay at its zero value, got %+v", cfg)
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1 (the implicit default when there's no config file at all)", cfg.Version)
	}
}

func TestLoad_UnknownKeyRejected(t *testing.T) {
	path := writeConfigFile(t, `
duration: 5m
outptu: ./capture.kshrk
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for the unknown key 'outptu'")
	}
	if !strings.Contains(err.Error(), "outptu") {
		t.Errorf("error = %v, want it to name the unknown key 'outptu'", err)
	}
}

func TestLoad_WrongCaseKeyRejected(t *testing.T) {
	// "previouslogs" is a real, case-insensitively-matchable key one letter
	// away from valid — exactly the kind of typo #220 exists to catch. A
	// case-insensitive decode would silently accept this as previousLogs.
	path := writeConfigFile(t, `
duration: 5m
resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [default]
    interval: 30s
    previouslogs: true
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for the wrongly-cased key 'previouslogs'")
	}
	if !strings.Contains(err.Error(), "previouslogs") {
		t.Errorf("error = %v, want it to name the unknown key 'previouslogs'", err)
	}
}

func TestLoad_CorrectlyCasedKeyAccepted(t *testing.T) {
	path := writeConfigFile(t, `
duration: 5m
resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [default]
    interval: 30s
    previousLogs: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Resources[0].PreviousLogs {
		t.Error("expected PreviousLogs=true")
	}
}

func TestLoad_LegacyTopLevelKeyAlias(t *testing.T) {
	path := writeConfigFile(t, `
duration: 5m
auto_discover: true
auto_discover_exclude_groups: ["metrics.k8s.io"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoDiscover {
		t.Error("expected AutoDiscover=true from legacy auto_discover key")
	}
	if len(cfg.AutoDiscoverExcludeGroups) != 1 || cfg.AutoDiscoverExcludeGroups[0] != "metrics.k8s.io" {
		t.Errorf("unexpected AutoDiscoverExcludeGroups: %v", cfg.AutoDiscoverExcludeGroups)
	}
}

func TestLoad_LegacyNestedKeyAlias(t *testing.T) {
	path := writeConfigFile(t, `
duration: 5m
ui:
  port: "8080"
  api_port: "8081"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UI.Port != "8080" {
		t.Errorf("UI.Port = %q, want 8080", cfg.UI.Port)
	}
	if cfg.UI.APIPort != "8081" {
		t.Errorf("UI.APIPort = %q, want 8081 (from legacy api_port)", cfg.UI.APIPort)
	}
}

func TestLoad_CanonicalKeyWinsOverLegacyAlias(t *testing.T) {
	path := writeConfigFile(t, `
duration: 5m
auto_discover: false
autoDiscover: true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoDiscover {
		t.Error("expected the canonical autoDiscover=true to win over the legacy auto_discover=false")
	}
}

func TestLoad_MissingVersionDefaultsToOne(t *testing.T) {
	path := writeConfigFile(t, `duration: 5m`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1 (the implicit default for a config with no version: key)", cfg.Version)
	}
}

func TestLoad_ExplicitVersionZeroRejected(t *testing.T) {
	path := writeConfigFile(t, `
version: 0
duration: 5m
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an explicit version: 0 (0 is not a valid schema version)")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %v, want it to say the version is invalid", err)
	}
}

func TestLoad_NegativeVersionRejected(t *testing.T) {
	path := writeConfigFile(t, `
version: -1
duration: 5m
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a negative version")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %v, want it to say the version is invalid", err)
	}
}

func TestLoad_VersionTooNew(t *testing.T) {
	path := writeConfigFile(t, `
version: 999
duration: 5m
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a config version newer than this build understands")
	}
	if !strings.Contains(err.Error(), "upgrade kshrk") {
		t.Errorf("error = %v, want it to suggest upgrading kshrk", err)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	// Mirrors cmd/root.go's initConfig wiring, scoped to this test via the
	// global viper singleton (config.Load reads it for env/flag overrides
	// only — see Load's doc comment). Reset afterward so this doesn't leak
	// SetEnvPrefix/AutomaticEnv state into other tests in this package,
	// whose execution order isn't guaranteed.
	t.Cleanup(viper.Reset)
	viper.SetEnvPrefix("KSHRK")
	viper.AutomaticEnv()
	t.Setenv("KSHRK_DURATION", "99h")
	t.Setenv("KSHRK_OUTPUT", "from-env.kshrk")

	path := writeConfigFile(t, `
duration: 5m
output: ./capture.kshrk
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DurationRaw != "99h" {
		t.Errorf("DurationRaw = %q, want 99h (from KSHRK_DURATION)", cfg.DurationRaw)
	}
	if cfg.Output != "from-env.kshrk" {
		t.Errorf("Output = %q, want from-env.kshrk (from KSHRK_OUTPUT)", cfg.Output)
	}
}

func TestLoad_UnprefixedEnvVarIgnored(t *testing.T) {
	// See TestLoad_EnvOverride: reset afterward so SetEnvPrefix/AutomaticEnv
	// doesn't leak into other tests in this package.
	t.Cleanup(viper.Reset)
	viper.SetEnvPrefix("KSHRK")
	viper.AutomaticEnv()
	t.Setenv("DURATION", "99h") // bare, unprefixed — must NOT override (#218)

	path := writeConfigFile(t, `duration: 5m`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DurationRaw != "5m" {
		t.Errorf("DurationRaw = %q, want 5m (unprefixed DURATION must not apply)", cfg.DurationRaw)
	}
}

func validatedCfg(t *testing.T, dur string, resources []Resource) *Config {
	t.Helper()
	cfg := &Config{DurationRaw: dur, Output: "/tmp/k8shark-test-out.kshrk", Resources: resources}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

func TestWarnings_Clean(t *testing.T) {
	cfg := validatedCfg(t, "10m", []Resource{
		{Resource: "pods", Version: "v1", IntervalRaw: "30s"},
	})
	if ws := Warnings(cfg); len(ws) != 0 {
		t.Errorf("expected no warnings, got: %v", ws)
	}
}

func TestWarnings_LongDuration(t *testing.T) {
	cfg := validatedCfg(t, "3h", []Resource{
		{Resource: "pods", Version: "v1", IntervalRaw: "30s"},
	})
	ws := Warnings(cfg)
	if len(ws) == 0 {
		t.Fatal("expected warning for long duration, got none")
	}
	found := false
	for _, w := range ws {
		if contains(w, "very long") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'very long' warning, got: %v", ws)
	}
}

func TestWarnings_ShortInterval(t *testing.T) {
	cfg := validatedCfg(t, "10m", []Resource{
		{Resource: "pods", Version: "v1", IntervalRaw: "2s", Interval: 2 * time.Second},
	})
	// Manually set Interval since Validate parses from IntervalRaw.
	cfg.Resources[0].Interval = 2 * time.Second
	ws := Warnings(cfg)
	found := false
	for _, w := range ws {
		if contains(w, "very short") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'very short' interval warning, got: %v", ws)
	}
}

func TestWarnings_ClusterScopedWithNamespaces(t *testing.T) {
	cfg := validatedCfg(t, "10m", []Resource{
		{Resource: "nodes", Version: "v1", IntervalRaw: "30s", Namespaces: []string{"default"}},
	})
	ws := Warnings(cfg)
	found := false
	for _, w := range ws {
		if contains(w, "cluster-scoped") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected cluster-scoped namespace warning, got: %v", ws)
	}
}

// TestWarnings_BuiltinGroupClusterScopedWithNamespaces guards a gap #240's
// fix introduced: a cluster-scoped resource in a known builtin group (so it's
// excluded from the "non-core resource, might be a CRD" advisory) but missing
// from knownClusterScoped would otherwise get *no* warning at all for
// namespaces: set — silently dropping the clearer cluster-scoped advisory
// instead of falling through to it.
func TestWarnings_BuiltinGroupClusterScopedWithNamespaces(t *testing.T) {
	cfg := validatedCfg(t, "10m", []Resource{
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1", Resource: "flowschemas", IntervalRaw: "30s", Namespaces: []string{"default"}},
	})
	ws := Warnings(cfg)
	found := false
	for _, w := range ws {
		if contains(w, "cluster-scoped") {
			found = true
		}
		if contains(w, "non-core resource") {
			t.Errorf("flowschemas is a known built-in resource, should not get the CRD advisory: %s", w)
		}
	}
	if !found {
		t.Errorf("expected cluster-scoped namespace warning for flowschemas, got: %v", ws)
	}
}

func TestWarnings_OutputExists(t *testing.T) {
	f, err := os.CreateTemp("", "k8shark-test-*.kshrk")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	cfg := validatedCfg(t, "10m", []Resource{
		{Resource: "pods", Version: "v1", IntervalRaw: "30s"},
	})
	cfg.Output = f.Name()
	ws := Warnings(cfg)
	found := false
	for _, w := range ws {
		if contains(w, "already exists") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected output-already-exists warning, got: %v", ws)
	}
}

// TestWarnings_BuiltinGroupWithNamespaces_NoWarn locks in #240: apps/batch/etc.
// are never CRD-backed, so a perfectly ordinary namespaced resource in one of
// them (e.g. apps/v1 Deployments) must not trigger the CRD-namespace advisory
// just because its group is non-core. This was firing for every non-core
// built-in resource in the shipped examples/k8shark.yaml.
func TestWarnings_BuiltinGroupWithNamespaces_NoWarn(t *testing.T) {
	cfg := validatedCfg(t, "10m", []Resource{
		{Group: "apps", Version: "v1", Resource: "deployments", IntervalRaw: "30s", Namespaces: []string{"default"}},
		{Group: "batch", Version: "v1", Resource: "jobs", IntervalRaw: "30s", Namespaces: []string{"default"}},
	})
	ws := Warnings(cfg)
	for _, w := range ws {
		if contains(w, "non-core resource") {
			t.Errorf("unexpected non-core advisory for a well-known built-in group: %s", w)
		}
	}
}

func TestIsClusterScoped(t *testing.T) {
	for _, r := range []string{"nodes", "persistentvolumes", "storageclasses", "namespaces"} {
		if !IsClusterScoped(r) {
			t.Errorf("expected %q to be cluster-scoped", r)
		}
	}
	for _, r := range []string{"pods", "deployments", "services", "configmaps"} {
		if IsClusterScoped(r) {
			t.Errorf("expected %q NOT to be cluster-scoped", r)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func TestWarnings_CRDWithNamespaces(t *testing.T) {
	cfg := validatedCfg(t, "10m", []Resource{
		{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers", IntervalRaw: "30s", Namespaces: []string{"default"}},
	})
	ws := Warnings(cfg)
	found := false
	for _, w := range ws {
		if contains(w, "non-core resource") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected non-core CRD namespace advisory warning, got: %v", ws)
	}
}

func TestWarnings_CRDNoNamespaces_NoWarn(t *testing.T) {
	// A non-core resource without namespaces set should NOT trigger the advisory.
	cfg := validatedCfg(t, "10m", []Resource{
		{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers", IntervalRaw: "30s"},
	})
	ws := Warnings(cfg)
	for _, w := range ws {
		if contains(w, "non-core resource") {
			t.Errorf("unexpected non-core advisory for resource without namespaces: %s", w)
		}
	}
}

func TestResourceDedupEnabled_DefaultTrue(t *testing.T) {
	r := Resource{}
	if !r.DedupEnabled() {
		t.Fatal("expected default dedup enabled when dedup is unset")
	}
}

func TestResourceDedupEnabled_ExplicitFalse(t *testing.T) {
	val := false
	r := Resource{Dedup: &val}
	if r.DedupEnabled() {
		t.Fatal("expected dedup disabled when dedup is explicitly false")
	}
}

func TestValidate_AllDirective_NoResourceFieldsRequired(t *testing.T) {
	cfg := &Config{
		DurationRaw: "10m",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources: []Resource{{
			All:        true,
			Scope:      "namespaced",
			Namespaces: []string{"default"},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Resources[0].Interval; got != 30*time.Second {
		t.Fatalf("expected default interval 30s, got %s", got)
	}
}

func TestValidate_AllDirective_InvalidScope(t *testing.T) {
	cfg := &Config{
		DurationRaw: "10m",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources: []Resource{{
			All:   true,
			Scope: "invalid-scope",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid all=true scope, got nil")
	}
}

func TestValidate_DiscoveryStartup_TooShortDuration_WithAllDirective(t *testing.T) {
	cfg := &Config{
		DurationRaw: "2s",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources: []Resource{{
			All: true,
		}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected duration guard error, got nil")
	}
	if !contains(err.Error(), "duration") || !contains(err.Error(), "too short") {
		t.Fatalf("expected short-duration guard error, got: %v", err)
	}
}

func TestValidate_DiscoveryStartup_TooShortDuration_WithWildcardNamespaces(t *testing.T) {
	cfg := &Config{
		DurationRaw: "2s",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources: []Resource{{
			Version:    "v1",
			Resource:   "pods",
			Namespaces: []string{"*"},
		}},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected duration guard error, got nil")
	}
	if !contains(err.Error(), "duration") || !contains(err.Error(), "too short") {
		t.Fatalf("expected short-duration guard error, got: %v", err)
	}
}

func TestValidate_DiscoveryStartup_NormalResource_AllowsShortDuration(t *testing.T) {
	cfg := &Config{
		DurationRaw: "2s",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources: []Resource{{
			Version:  "v1",
			Resource: "pods",
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected short duration without discovery startup to validate, got: %v", err)
	}
}

func TestRedactionConfig_ParsesCorrectly(t *testing.T) {
	cfg := &Config{
		DurationRaw: "5m",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources:   []Resource{{Resource: "pods", Version: "v1", IntervalRaw: "30s"}},
		Redaction: RedactionConfig{
			RedactSecrets: true,
			AllowSecrets:  []string{"default/app-secret"},
			Rules: []RedactionRule{
				{
					FieldPath:     "data.api-key",
					Kind:          "ConfigMap",
					LabelSelector: "app=sensitive",
					Replacement:   "REDACTED",
					ValueType:     "string",
				},
				{
					FieldPath:   "spec.containers[*].env[*].value",
					Kind:        "Pod",
					Replacement: "REDACTED",
				},
				{
					FieldPath:   "**.password",
					Kind:        "*",
					Replacement: "REDACTED",
				},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Redaction.RedactSecrets {
		t.Error("expected RedactSecrets=true")
	}
	if len(cfg.Redaction.AllowSecrets) != 1 || cfg.Redaction.AllowSecrets[0] != "default/app-secret" {
		t.Errorf("unexpected AllowSecrets: %v", cfg.Redaction.AllowSecrets)
	}
	if len(cfg.Redaction.Rules) != 3 {
		t.Fatalf("expected 3 redaction rules, got %d", len(cfg.Redaction.Rules))
	}
	if cfg.Redaction.Rules[0].Kind != "ConfigMap" {
		t.Errorf("expected kind ConfigMap, got %q", cfg.Redaction.Rules[0].Kind)
	}
	if cfg.Redaction.Rules[0].LabelSelector != "app=sensitive" {
		t.Errorf("expected labelSelector app=sensitive, got %q", cfg.Redaction.Rules[0].LabelSelector)
	}
	if cfg.Redaction.Rules[0].ValueType != "string" {
		t.Errorf("expected valueType string, got %q", cfg.Redaction.Rules[0].ValueType)
	}
}

func TestRedactionConfig_EmptyIsValid(t *testing.T) {
	cfg := &Config{
		DurationRaw: "5m",
		Output:      "/tmp/k8shark-test-out.kshrk",
		Resources:   []Resource{{Resource: "pods", Version: "v1", IntervalRaw: "30s"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// zero-value RedactionConfig is fine
	if cfg.Redaction.RedactSecrets {
		t.Error("expected RedactSecrets=false by default")
	}
	if len(cfg.Redaction.Rules) != 0 {
		t.Errorf("expected no rules by default, got %d", len(cfg.Redaction.Rules))
	}
}
