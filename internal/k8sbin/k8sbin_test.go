package k8sbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUniqueTempPath verifies concurrent EnsureControllerManager calls (e.g.
// two kshrk processes downloading/building the same version at once) get
// distinct temp files rather than racing through a shared fixed name.
func TestUniqueTempPath(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		p, err := uniqueTempPath(dir)
		if err != nil {
			t.Fatalf("uniqueTempPath: %v", err)
		}
		if filepath.Dir(p) != dir {
			t.Fatalf("uniqueTempPath returned %q, want it inside %q (same filesystem for atomic rename)", p, dir)
		}
		if seen[p] {
			t.Fatalf("uniqueTempPath returned a duplicate path: %q", p)
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("uniqueTempPath's file doesn't exist: %v", err)
		}
	}
}

func TestHasPrebuiltBinary(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         bool
	}{
		{"linux", "amd64", true},
		{"linux", "arm64", true},
		{"linux", "386", false},     // Kubernetes doesn't publish this either
		{"linux", "ppc64le", false}, // ...nor this
		{"linux", "s390x", false},   // ...nor this
		{"darwin", "amd64", false},
		{"darwin", "arm64", false},
		{"windows", "amd64", false},
	}
	for _, tc := range cases {
		if got := hasPrebuiltBinary(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("hasPrebuiltBinary(%q, %q) = %v, want %v", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestExtractTarGz_PathTraversalRejected locks in extractTarGz's zip-slip
// guard across a range of traversal shapes: none of these entries may ever
// land outside destDir, and none should silently collide into some other
// remapped path inside destDir either — the entry is simply skipped.
func TestExtractTarGz_PathTraversalRejected(t *testing.T) {
	cases := []struct {
		name     string
		wantSafe bool
	}{
		{"go.mod", true},
		{"cmd/kube-apiserver/main.go", true},
		{"..", false},
		{"../etc/passwd", false},
		{"../../etc/passwd", false},
		{"foo/../../bar", false},
		{"/etc/passwd", false},
		{"foo/../bar", true}, // cleans to "bar", still inside destDir
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			content := "payload"
			if err := tw.WriteHeader(&tar.Header{Name: tc.name, Mode: 0o644, Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
			tw.Close()
			gz.Close()

			tmp := t.TempDir()
			tarPath := filepath.Join(tmp, "archive.tar.gz")
			if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			destDir := filepath.Join(tmp, "out")
			if err := extractTarGz(tarPath, destDir); err != nil {
				t.Fatalf("extractTarGz: %v", err)
			}

			// Walk the whole tmp dir (destDir's parent) for any file carrying
			// our marker content: this catches both "did a safe entry get
			// extracted" and "did an unsafe one escape destDir" in one pass,
			// since tmp is an isolated t.TempDir() a traversal within a few
			// ".."s still lands under.
			foundInside, foundOutside := false, false
			_ = filepath.Walk(tmp, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				b, rerr := os.ReadFile(p)
				if rerr != nil || string(b) != content {
					return nil
				}
				rel, rerr := filepath.Rel(destDir, p)
				if rerr == nil && !strings.HasPrefix(rel, "..") {
					foundInside = true
				} else {
					foundOutside = true
				}
				return nil
			})
			if foundOutside {
				t.Fatalf("entry %q escaped destDir", tc.name)
			}
			if foundInside != tc.wantSafe {
				t.Errorf("entry %q extracted inside destDir = %v, want %v", tc.name, foundInside, tc.wantSafe)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"v1.36.1", "v1.36.1", false},
		{"v1.30.0-rc.1", "v1.30.0-rc.1", false},
		{"v1.28.5+vmware.1", "v1.28.5", false}, // build metadata stripped
		{"unknown", "", true},
		{"1.36.1", "", true},    // missing leading "v"
		{"v1.36", "", true},     // not a full semver
		{"../../etc", "", true}, // path traversal attempt
		{"v1.2.3; rm -rf", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeVersion(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeVersion(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDownloadAndVerifyToFile(t *testing.T) {
	content := []byte("pretend kube-controller-manager binary contents")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/good/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(content) })
	mux.HandleFunc("/good/bin.sha256", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sumHex)) })
	mux.HandleFunc("/good/bin.sha256-withname", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sumHex + "  bin\n"))
	})
	mux.HandleFunc("/bad/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(content) })
	mux.HandleFunc("/bad/bin.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("checksum matches", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out")
		if err := downloadAndVerifyToFile(srv.URL+"/good/bin", dest); err != nil {
			t.Fatalf("downloadAndVerifyToFile: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("content mismatch")
		}
	})

	t.Run("checksum mismatch removes dest", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "out")
		err := downloadAndVerifyToFile(srv.URL+"/bad/bin", dest)
		if err == nil {
			t.Fatalf("expected checksum mismatch error, got nil")
		}
		if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Errorf("error = %v, want checksum mismatch", err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Errorf("dest file should have been removed after checksum mismatch")
		}
	})
}

// buildTarGz packs the given name->content map into a gzip-compressed tar
// archive and returns its bytes.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractTarGz(t *testing.T) {
	t.Run("extracts regular files and directories", func(t *testing.T) {
		data := buildTarGz(t, map[string]string{
			"go.mod":           "module k8s.io/kubernetes\n",
			"cmd/kube-cm/x.go": "package main\n",
		})
		tmp := t.TempDir()
		tarPath := filepath.Join(tmp, "archive.tar.gz")
		if err := os.WriteFile(tarPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		destDir := filepath.Join(tmp, "out")
		if err := extractTarGz(tarPath, destDir); err != nil {
			t.Fatalf("extractTarGz: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(destDir, "go.mod"))
		if err != nil {
			t.Fatalf("reading extracted go.mod: %v", err)
		}
		if string(got) != "module k8s.io/kubernetes\n" {
			t.Errorf("go.mod content = %q", got)
		}
		if _, err := os.Stat(filepath.Join(destDir, "cmd/kube-cm/x.go")); err != nil {
			t.Errorf("nested file missing: %v", err)
		}
	})

	t.Run("rejects path traversal entries", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		evil := "../../../tmp/kshark-extract-escape-test"
		content := "escaped!"
		if err := tw.WriteHeader(&tar.Header{Name: evil, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()

		tmp := t.TempDir()
		tarPath := filepath.Join(tmp, "evil.tar.gz")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		destDir := filepath.Join(tmp, "out")
		if err := extractTarGz(tarPath, destDir); err != nil {
			t.Fatalf("extractTarGz: %v", err)
		}
		if _, err := os.Stat("/tmp/kshark-extract-escape-test"); !os.IsNotExist(err) {
			t.Fatalf("path traversal entry escaped destDir")
			_ = os.Remove("/tmp/kshark-extract-escape-test")
		}
	})

	t.Run("skips symlink entries", func(t *testing.T) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{
			Name: "evil-link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
		}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()

		tmp := t.TempDir()
		tarPath := filepath.Join(tmp, "symlink.tar.gz")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		destDir := filepath.Join(tmp, "out")
		if err := extractTarGz(tarPath, destDir); err != nil {
			t.Fatalf("extractTarGz: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(destDir, "evil-link")); !os.IsNotExist(err) {
			t.Errorf("symlink entry should have been skipped, not created")
		}
	})
}

func TestIsExecutableFile(t *testing.T) {
	tmp := t.TempDir()
	notExec := filepath.Join(tmp, "notexec")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isExecutableFile(notExec) {
		t.Errorf("0o644 file reported executable")
	}

	exec := filepath.Join(tmp, "exec")
	if err := os.WriteFile(exec, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(exec) {
		t.Errorf("0o755 file not reported executable")
	}

	if isExecutableFile(filepath.Join(tmp, "missing")) {
		t.Errorf("missing file reported executable")
	}

	if isExecutableFile(tmp) {
		t.Errorf("directory reported executable")
	}
}
