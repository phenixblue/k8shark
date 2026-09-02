package anonymize

import "testing"

func upper(s string) string { return s + "-ALIASED" }

// noExclusions is the excludedFunc used by every test that isn't
// specifically exercising rule-based exclusion — equivalent to an archive
// run with no configured AnonymizeRule entries.
func noExclusions(Category, string, string) bool { return false }

func TestRewriteNamespaceInPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{
			name:   "namespace GET",
			path:   "/api/v1/namespaces/prod",
			want:   "/api/v1/namespaces/prod-ALIASED",
			wantOK: true,
		},
		{
			name:   "namespaced list",
			path:   "/api/v1/namespaces/prod/pods",
			want:   "/api/v1/namespaces/prod-ALIASED/pods",
			wantOK: true,
		},
		{
			name:   "namespaced object GET",
			path:   "/api/v1/namespaces/prod/pods/web-1",
			want:   "/api/v1/namespaces/prod-ALIASED/pods/web-1",
			wantOK: true,
		},
		{
			name:   "apis group/version form",
			path:   "/apis/apps/v1/namespaces/prod/deployments",
			want:   "/apis/apps/v1/namespaces/prod-ALIASED/deployments",
			wantOK: true,
		},
		{
			name:   "apis group/version object GET",
			path:   "/apis/apps/v1/namespaces/prod/deployments/web",
			want:   "/apis/apps/v1/namespaces/prod-ALIASED/deployments/web",
			wantOK: true,
		},
		{
			name:   "subresource path — only the namespace segment moves",
			path:   "/api/v1/namespaces/prod/pods/web-1/log",
			want:   "/api/v1/namespaces/prod-ALIASED/pods/web-1/log",
			wantOK: true,
		},
		{
			name:   "cluster-scoped — no namespaces segment at all",
			path:   "/api/v1/nodes",
			want:   "/api/v1/nodes",
			wantOK: false,
		},
		{
			name:   "cluster-scoped object GET",
			path:   "/api/v1/nodes/worker-1",
			want:   "/api/v1/nodes/worker-1",
			wantOK: false,
		},
		{
			name:   "all-namespaces list — 'pods' is not 'namespaces'",
			path:   "/api/v1/pods",
			want:   "/api/v1/pods",
			wantOK: false,
		},
		{
			name:   "the namespace collection endpoint itself — nothing follows 'namespaces'",
			path:   "/api/v1/namespaces",
			want:   "/api/v1/namespaces",
			wantOK: false,
		},
		{
			name:   "trailing slash after namespaces — empty segment, nothing to alias",
			path:   "/api/v1/namespaces/",
			want:   "/api/v1/namespaces/",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rewriteNamespaceInPath(tc.path, upper)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("rewriteNamespaceInPath(%q) = (%q, %v), want (%q, %v)",
					tc.path, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Only the namespace segment must change — replacing by segment *position*,
// not by a blind string substitution of the original namespace value
// wherever it happens to appear in the path. Aliasing to a value that
// collides with the resource-type segment's own literal text ("pods") is
// exactly the case that would expose a blind-replace implementation: it
// would turn the resource segment into something else too, or replace both
// occurrences, instead of only the one namespace-segment position.
func TestRewriteNamespaceInPath_ReplacesByPositionNotBySubstring(t *testing.T) {
	alias := func(string) string { return "pods" }
	got, ok := rewriteNamespaceInPath("/api/v1/namespaces/prod/pods", alias)
	want := "/api/v1/namespaces/pods/pods"
	if !ok || got != want {
		t.Fatalf("rewriteNamespaceInPath(...) = (%q, %v), want (%q, true)", got, ok, want)
	}
}
