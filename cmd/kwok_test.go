package cmd

import (
	"strings"
	"testing"
)

// TestStartKwok_NotInstalled: with no kwok on PATH, startKwok fails with an
// actionable install hint rather than launching anything.
func TestStartKwok_NotInstalled(t *testing.T) {
	t.Setenv("PATH", "") // ensure exec.LookPath("kwok") fails
	KwokStages = []byte("dummy")

	cleanup, err := startKwok("/tmp/does-not-matter.yaml")
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected an error when kwok is not on PATH")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error = %q, want an install hint mentioning PATH", err.Error())
	}
}
