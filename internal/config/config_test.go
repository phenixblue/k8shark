package config

import (
	"os"
	"testing"
	"time"
)

func validatedCfg(t *testing.T, dur string, resources []Resource) *Config {
	t.Helper()
	cfg := &Config{DurationRaw: dur, Output: "/tmp/k8shark-test-out.tar.gz", Resources: resources}
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

func TestWarnings_OutputExists(t *testing.T) {
	f, err := os.CreateTemp("", "k8shark-test-*.tar.gz")
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
