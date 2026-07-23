package query

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func TestSearchText_SubstringMatchInObjectBody(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods": `{"kind":"PodList","apiVersion":"v1","items":[
		  {"metadata":{"name":"web-1","namespace":"prod","annotations":{"note":"needs a db migration before rollout"}}}
		]}`,
	})

	result, err := SearchText(store, TextOptions{Pattern: "db migration"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("expected 1 match, got %+v", result.Matches)
	}
	m := result.Matches[0]
	if m.Field != "metadata.annotations.note" {
		t.Errorf("expected field path metadata.annotations.note, got %q", m.Field)
	}
	if m.Name != "web-1" || m.Namespace != "prod" || m.Resource != "pods" {
		t.Errorf("unexpected object identity: %+v", m)
	}
}

func TestSearchText_RegexMatch(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods": `{"kind":"PodList","apiVersion":"v1","items":[
		  {"metadata":{"name":"web-1"},"spec":{"containers":[{"image":"nginx:1.25-alpine"}]}}
		]}`,
	})

	result, err := SearchText(store, TextOptions{Pattern: `nginx:1\.\d+`, Regex: true})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Field != "spec.containers[0].image" {
		t.Fatalf("expected one match on spec.containers[0].image, got %+v", result.Matches)
	}
}

func TestSearchText_InvalidRegex(t *testing.T) {
	store := buildQueryStore(t, map[string]string{})
	if _, err := SearchText(store, TextOptions{Pattern: "(unclosed", Regex: true}); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestSearchText_EmptyPattern(t *testing.T) {
	store := buildQueryStore(t, map[string]string{})
	if _, err := SearchText(store, TextOptions{Pattern: ""}); err == nil {
		t.Error("expected error for empty substring pattern")
	}
}

func TestSearchText_EmptyRegexPattern(t *testing.T) {
	// regexp.Compile("") succeeds and matches everything at position 0 — an
	// empty pattern must be rejected before it reaches regexp.Compile, in
	// both modes, not just substring mode.
	store := buildQueryStore(t, map[string]string{})
	if _, err := SearchText(store, TextOptions{Pattern: "", Regex: true}); err == nil {
		t.Error("expected error for empty regex pattern")
	}
}

func TestSearchText_MatchesCurrentAndPreviousLogs(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/crash-demo/pods/flaky-1/log?container=worker": mustJSONString(t,
			"starting up\nfatal: connection refused to db:5432\n"),
		"/api/v1/namespaces/crash-demo/pods/flaky-1/log?container=worker&previous=true": mustJSONString(t,
			"starting up\nfatal: connection refused to db:5432 (attempt 2)\n"),
	})

	result, err := SearchText(store, TextOptions{Pattern: "connection refused"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("expected 2 matches (current + previous), got %+v", result.Matches)
	}
	sawCurrent, sawPrevious := false, false
	for _, m := range result.Matches {
		if m.Container != "worker" || m.Namespace != "crash-demo" || m.Name != "flaky-1" {
			t.Errorf("unexpected log match identity: %+v", m)
		}
		if m.Previous {
			sawPrevious = true
		} else {
			sawCurrent = true
		}
	}
	if !sawCurrent || !sawPrevious {
		t.Errorf("expected both current and previous log matches, got %+v", result.Matches)
	}
}

func TestSearchText_ResourceFilterExcludesLogs(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/crash-demo/pods/flaky-1/log?container=worker": mustJSONString(t, "connection refused"),
		"/api/v1/namespaces/crash-demo/services":                          `{"kind":"ServiceList","apiVersion":"v1","items":[{"metadata":{"name":"connection-svc"}}]}`,
	})

	result, err := SearchText(store, TextOptions{Pattern: "connection", Resource: "services"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Resource != "services" {
		t.Fatalf("expected only the services match, got %+v", result.Matches)
	}
}

func TestSearchText_NamespaceFilterAppliesToLogs(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods/a/log?container=c": mustJSONString(t, "boom"),
		"/api/v1/namespaces/dev/pods/b/log?container=c":  mustJSONString(t, "boom"),
	})

	result, err := SearchText(store, TextOptions{Pattern: "boom", Namespace: "dev"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Namespace != "dev" {
		t.Fatalf("expected only the dev namespace log match, got %+v", result.Matches)
	}
}

func TestSearchText_IncludesPaginatedListPaths(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces?limit=500": `{"kind":"NamespaceList","apiVersion":"v1","items":[{"metadata":{"name":"prod-east"}}]}`,
	})

	result, err := SearchText(store, TextOptions{Pattern: "prod-east"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Resource != "namespaces" {
		t.Fatalf("expected the paginated namespaces list to be searched, got %+v", result.Matches)
	}
}

func TestSearchText_SkipsTableViews(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods":          `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"needle-pod"}}]}`,
		"/api/v1/namespaces/prod/pods?as=Table": `{"kind":"Table","rows":[{"cells":["needle-pod"]}]}`,
	})

	result, err := SearchText(store, TextOptions{Pattern: "needle-pod"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("expected only the plain pods list to match, got %+v", result.Matches)
	}
}

func TestSearchText_NoMatches(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods": `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`,
	})

	result, err := SearchText(store, TextOptions{Pattern: "not-present-anywhere"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("expected no matches, got %+v", result.Matches)
	}
}

func TestSnippet_DoesNotSplitMultiByteRune(t *testing.T) {
	// "€" is 3 bytes in UTF-8, so a 40-byte radius lands mid-rune (40 isn't a
	// multiple of 3) unless the cut point is snapped to a rune boundary.
	euro := "€"
	prefix := strings.Repeat(euro, 20)
	suffix := strings.Repeat(euro, 20)
	s := prefix + "TARGET" + suffix
	start := len(prefix)
	end := start + len("TARGET")

	out := snippet(s, start, end)
	if !utf8.ValidString(out) {
		t.Fatalf("snippet produced invalid UTF-8: %q", out)
	}
	if !strings.Contains(out, "TARGET") {
		t.Fatalf("snippet lost the match: %q", out)
	}
}
