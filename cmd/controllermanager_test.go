package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kube-controller-manager's output used to go straight to kshrk's stdout/stderr,
// where one retrying controller produced 5 MB in a minute and buried the replay
// banner (#329). resolveControllerLog moves it aside without discarding it.
func TestResolveControllerLog(t *testing.T) {
	t.Run("dash streams inline", func(t *testing.T) {
		w, closeFn, shown, err := resolveControllerLog("-")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		defer closeFn()
		if w != os.Stderr {
			t.Errorf("writer = %v, want os.Stderr", w)
		}
		if shown != "stderr" {
			t.Errorf("shown = %q, want stderr", shown)
		}
	})

	t.Run("explicit path is written and truncated", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kcm.log")
		if err := os.WriteFile(path, []byte("stale content from a previous run"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		w, closeFn, shown, err := resolveControllerLog(path)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if shown != path {
			t.Errorf("shown = %q, want %q", shown, path)
		}
		if _, err := w.Write([]byte("fresh")); err != nil {
			t.Fatalf("write: %v", err)
		}
		closeFn()

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "fresh" {
			t.Errorf("file = %q, want %q — O_TRUNC should drop the previous run's log", got, "fresh")
		}
	})

	t.Run("empty picks a temp file whose path is reported", func(t *testing.T) {
		w, closeFn, shown, err := resolveControllerLog("")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if _, werr := w.Write([]byte("x")); werr != nil {
			t.Fatalf("write: %v", werr)
		}
		closeFn()
		defer os.Remove(shown)

		// The path has to be reportable: a default that silently swallowed the
		// output would make a misbehaving controller undebuggable.
		if !strings.Contains(filepath.Base(shown), "kshrk-controller-manager-") {
			t.Errorf("temp path = %q, want it to identify itself", shown)
		}
		if _, serr := os.Stat(shown); serr != nil {
			t.Errorf("reported path does not exist: %v", serr)
		}
	})

	t.Run("unwritable path is an error, not a silent fallback", func(t *testing.T) {
		if _, _, _, err := resolveControllerLog(filepath.Join(t.TempDir(), "no-such-dir", "kcm.log")); err == nil {
			t.Error("expected an error for an unwritable destination")
		}
	})
}
