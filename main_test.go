package main

import (
	"strings"
	"testing"
)

// TestKwokStagesEmbedded verifies examples/kwok-stages.yaml embedded into the
// binary (used by `replay --with-kwok`) — a build/embed regression guard.
func TestKwokStagesEmbedded(t *testing.T) {
	if len(kwokStages) == 0 {
		t.Fatal("examples/kwok-stages.yaml did not embed")
	}
	if n := strings.Count(string(kwokStages), "kind: Stage"); n != 4 {
		t.Errorf("embedded stages: found %d Stage docs, want 4", n)
	}
}
