package v2

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAllMountedHandlersHaveTests guards against a new handler being registered
// in v2.go's Mount without any test referencing it. It scans v2.go for the
// h.serveXxx handlers wired into the mux and asserts each name appears in at
// least one *_test.go file in this package.
func TestAllMountedHandlersHaveTests(t *testing.T) {
	src, err := os.ReadFile("v2.go")
	if err != nil {
		t.Fatalf("read v2.go: %v", err)
	}
	matches := regexp.MustCompile(`h\.(serve\w+)`).FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no h.serveXxx handlers in v2.go — did Mount move?")
	}
	handlers := map[string]bool{}
	for _, m := range matches {
		handlers[m[1]] = true
	}

	testFiles, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob test files: %v", err)
	}
	var corpus strings.Builder
	for _, f := range testFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		corpus.Write(b)
	}
	all := corpus.String()

	for name := range handlers {
		if !strings.Contains(all, name) {
			t.Errorf("handler %q is registered in v2.go's Mount but no test references it", name)
		}
	}
}
