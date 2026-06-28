package v2

import (
	"strings"

	"github.com/phenixblue/k8shark/internal/diagnose"
)

// Pod-health classification lives in internal/diagnose so the CLI and UI share
// one implementation. These aliases keep the v2 call sites unchanged.
type (
	PodHealth       = diagnose.PodHealth
	ContainerHealth = diagnose.ContainerHealth
)

// ClassifyPod parses a pod body and returns its PodHealth.
func ClassifyPod(body []byte) PodHealth { return diagnose.ClassifyPod(body) }

// PodNamePrefix groups pods that share an owning ReplicaSet/Deployment by
// stripping the trailing hash segment(s). "kubevirt-migration-controller-
// 6c77fbf565-2rz8h" → "kubevirt-migration-controller-*". When the name has
// no recognizable hash suffix the full name is returned with a "-*" suffix.
func PodNamePrefix(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name + "-*"
	}
	// Heuristic: strip from the right while the segment looks like a
	// pod-template hash (lowercase alphanumeric, length 5..10).
	trimmed := parts
	for len(trimmed) > 1 {
		last := trimmed[len(trimmed)-1]
		if !looksLikeHash(last) {
			break
		}
		trimmed = trimmed[:len(trimmed)-1]
	}
	if len(trimmed) == len(parts) {
		// Nothing stripped — fall back to dropping just the last segment.
		return strings.Join(parts[:len(parts)-1], "-") + "-*"
	}
	return strings.Join(trimmed, "-") + "-*"
}

func looksLikeHash(s string) bool {
	if len(s) < 4 || len(s) > 10 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') {
			return false
		}
	}
	// Pod-template hashes mix letters and digits.
	hasLetter, hasDigit := false, false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			hasLetter = true
		}
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}
