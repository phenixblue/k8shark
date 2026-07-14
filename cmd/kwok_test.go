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

// TestValidateKwokFlags: --with-kwok with the scheduling shim disabled is
// rejected (it would leave pods unscheduled and never reach Running).
func TestValidateKwokFlags(t *testing.T) {
	cases := []struct {
		withKwok, schedulePods, wantErr bool
	}{
		{false, true, false},  // no kwok, shim on
		{false, false, false}, // no kwok, shim off — fine
		{true, true, false},   // kwok + shim on — the supported combo
		{true, false, true},   // kwok + shim off — contradictory, rejected
	}
	for _, c := range cases {
		err := validateKwokFlags(c.withKwok, c.schedulePods)
		if (err != nil) != c.wantErr {
			t.Errorf("validateKwokFlags(withKwok=%v, schedulePods=%v) err=%v, wantErr=%v",
				c.withKwok, c.schedulePods, err, c.wantErr)
		}
	}
}
