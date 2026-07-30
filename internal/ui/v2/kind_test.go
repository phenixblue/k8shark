package v2

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/phenixblue/k8shark/internal/store"
)

func TestKindFromResource(t *testing.T) {
	cases := map[string]string{
		"pods":              "Pod",
		"configmaps":        "ConfigMap",
		"componentstatuses": "ComponentStatus",
		"endpoints":         "Endpoints",
		"networkpolicies":   "NetworkPolicy",
		"leases":            "Lease",
		// Not in client-go's built-in scheme: falls through to
		// store.ResourceToKind's naive depluralization heuristic, same as
		// every other caller of it (see internal/server/handler.go's
		// ResourceKind comment) — not something this package special-cases.
		"widgets": "Widget",
		"":        "",
	}
	for in, want := range cases {
		if got := kindFromResource(in); got != want {
			t.Errorf("kindFromResource(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestKindFromResource_AgreesWithStore guards #236: the UI previously kept a
// second, hand-maintained resource->Kind table (23 of 47 entries disagreed
// with the mock server's scheme-derived one). Walk every resource client-go's
// built-in scheme knows about — the same set internal/store derives its map
// from — and assert kindFromResource never disagrees with store.ResourceToKind.
// This fails immediately if issues.go ever grows its own table again.
func TestKindFromResource_AgreesWithStore(t *testing.T) {
	seen := map[string]bool{}
	for gvk := range clientgoscheme.Scheme.AllKnownTypes() {
		if gvk.Kind == "" || strings.HasSuffix(gvk.Kind, "List") || strings.HasSuffix(gvk.Kind, "Options") {
			continue
		}
		gvr, _ := meta.UnsafeGuessKindToResource(gvk)
		if gvr.Resource == "" || seen[gvr.Resource] {
			continue
		}
		seen[gvr.Resource] = true

		want := store.ResourceToKind(gvr.Resource)
		if got := kindFromResource(gvr.Resource); got != want {
			t.Errorf("kindFromResource(%q) = %q, want %q (store.ResourceToKind)", gvr.Resource, got, want)
		}
	}
	if len(seen) < 40 {
		t.Fatalf("only checked %d resources from the scheme, expected 40+ — scheme registration may be broken", len(seen))
	}
}
