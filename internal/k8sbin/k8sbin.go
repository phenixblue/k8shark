// Package k8sbin locates or produces a kube-controller-manager binary
// matching a capture's Kubernetes version, for --with-controller-manager.
//
// Kubernetes only publishes prebuilt server-component binaries (unlike
// kubectl) for linux/amd64 and linux/arm64. On other platforms there is no
// official download, so this package falls back to compiling
// kube-controller-manager from the official Kubernetes source tarball —
// the same "self-build" approach documented by the KWOK project for its own
// non-Linux binary gap
// (https://kwok.sigs.k8s.io/docs/user/kwokctl-platform-specific-binaries/).
// Both paths fetch only from dl.k8s.io (the official Kubernetes release CDN)
// and verify every download against its published SHA-256 checksum before
// it is executed or fed to a compiler.
package k8sbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	dlBaseURL       = "https://dl.k8s.io"
	binName         = "kube-controller-manager"
	downloadTimeout = 10 * time.Minute
)

// dlCheckRedirect rejects a redirect to a different host, or off HTTPS
// entirely, than the request that started the chain: every artifact this
// package fetches is served directly by dl.k8s.io over HTTPS with no
// redirect involved (verified empirically), so this is pure defense in
// depth against a compromised or malicious intermediate redirecting an
// otherwise-trusted download to an attacker-controlled host, or downgrading
// it to plaintext — same-host doesn't imply same-scheme, so both are
// checked. Exposed as its own function (rather than inlined in dlClient) so
// tests can pair it with a test server's own trusted Transport instead of
// dlClient's real one, which only trusts the public CA pool.
func dlCheckRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing to follow a non-HTTPS redirect to %s", req.URL)
	}
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refusing to follow redirect from %s to a different host %s", via[0].URL.Host, req.URL.Host)
	}
	return nil
}

// dlClient is the HTTP client used for every dl.k8s.io fetch (binary,
// source tarball, and their .sha256 companions).
var dlClient = &http.Client{CheckRedirect: dlCheckRedirect}

// versionRE matches a dl.k8s.io release version, e.g. "v1.36.1" or
// "v1.30.0-rc.1". Validated strictly before use in any URL or filesystem
// path: the version string originates from capture metadata (a field a
// crafted archive could set arbitrarily), so it must not be trusted as-is.
var versionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// EnsureControllerManager returns the path to a cached, executable
// kube-controller-manager binary matching gitVersion (as reported by a
// cluster's /version endpoint, e.g. "v1.36.1"). progress, if non-nil, is
// called with human-readable status lines as the binary is located, fetched,
// or built.
//
// On linux/{amd64,arm64} it downloads the official prebuilt release binary.
// On any other platform (no official binary exists) it downloads and builds
// the official Kubernetes source tarball via the host's Go toolchain
// ('go' must be on PATH). Results are cached under the user's cache
// directory, keyed by version and GOOS/GOARCH, so this only runs once per
// version per machine.
func EnsureControllerManager(gitVersion string, progress func(string)) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	version, err := normalizeVersion(gitVersion)
	if err != nil {
		return "", err
	}
	dir, err := binDir(version)
	if err != nil {
		return "", err
	}
	destPath := filepath.Join(dir, binName)
	if isExecutableFile(destPath) {
		progress(fmt.Sprintf("using cached kube-controller-manager %s (%s)", version, destPath))
		return destPath, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache directory %s: %w", dir, err)
	}

	if hasPrebuiltBinary(runtime.GOOS, runtime.GOARCH) {
		if err := downloadPrebuilt(version, destPath, progress); err != nil {
			return "", err
		}
	} else {
		if err := buildFromSource(version, destPath, progress); err != nil {
			return "", err
		}
	}
	return destPath, nil
}

// hasPrebuiltBinary reports whether dl.k8s.io publishes an official
// kube-controller-manager binary for goos/goarch. Kubernetes only builds
// server components for linux, and only for amd64 and arm64 among linux
// architectures — linux/386, linux/ppc64le, linux/s390x, etc. have no
// published binary either, so they must fall back to the source-build path
// just like darwin/windows do.
func hasPrebuiltBinary(goos, goarch string) bool {
	return goos == "linux" && (goarch == "amd64" || goarch == "arm64")
}

// normalizeVersion strips any "+build" metadata and validates the result
// against versionRE, rejecting anything that isn't a plausible dl.k8s.io
// release version before it's used to build a URL or filesystem path.
func normalizeVersion(raw string) (string, error) {
	v := raw
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	if !versionRE.MatchString(v) {
		return "", fmt.Errorf("capture's Kubernetes version %q doesn't look like a dl.k8s.io release version "+
			"(want vX.Y.Z); can't determine which kube-controller-manager to fetch", raw)
	}
	return v, nil
}

func binDir(version string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving cache directory: %w", err)
	}
	return filepath.Join(base, "k8shark", "kube-controller-manager", version, runtime.GOOS+"-"+runtime.GOARCH), nil
}

// isExecutableFile reports whether path is a regular file this package can
// hand to exec.Command. The Unix execute-bit check (mode&0o111) is
// meaningless on Windows — os.FileInfo.Mode() there reflects the read-only
// file attribute, not anything execution-related — so a cached Windows
// binary would never be recognized as such and EnsureControllerManager would
// redownload/rebuild on every call. A plain non-empty regular file is
// sufficient there: CreateProcess doesn't require an execute bit, and
// buildFromSource/downloadPrebuilt always write to this exact path (no
// implicit ".exe" is appended — go build's explicit -o path is honored as
// given even when cross-compiling to GOOS=windows).
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return info.Size() > 0
	}
	return info.Mode()&0o111 != 0
}

// uniqueTempPath returns a path to a new, empty, uniquely-named file in dir
// (the same directory destPath lives in, so a later os.Rename into destPath
// is atomic and same-filesystem). Using a random per-call name rather than a
// fixed "<dest>.download" means two concurrent EnsureControllerManager calls
// (e.g. two kshrk processes downloading/building the same version at once)
// can't corrupt each other by writing through the same path.
func uniqueTempPath(dir string) (string, error) {
	f, err := os.CreateTemp(dir, binName+"-*.download")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	return path, nil
}

// downloadPrebuilt fetches the official release binary for linux/GOARCH.
func downloadPrebuilt(version, destPath string, progress func(string)) error {
	arch := runtime.GOARCH
	url := fmt.Sprintf("%s/%s/bin/linux/%s/%s", dlBaseURL, version, arch, binName)
	progress(fmt.Sprintf("downloading kube-controller-manager %s for linux/%s", version, arch))

	tmpPath, err := uniqueTempPath(filepath.Dir(destPath))
	if err != nil {
		return err
	}
	if err := downloadAndVerifyToFile(url, tmpPath); err != nil {
		// uniqueTempPath's file may still be sitting there empty: a failure
		// before downloadAndVerifyToFile ever opens destPath (e.g. the
		// checksum fetch, or the artifact GET itself, failing) never reaches
		// its own cleanup path. Remove is a no-op if that path already did.
		_ = os.Remove(tmpPath)
		return fmt.Errorf("downloading kube-controller-manager: %w", err)
	}
	return atomicInstall(tmpPath, destPath)
}

// buildFromSource downloads the official Kubernetes source tarball and
// compiles kube-controller-manager with the host's Go toolchain, for
// platforms Kubernetes doesn't publish a server binary for.
func buildFromSource(version, destPath string, progress func(string)) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("--with-controller-manager: building kube-controller-manager from source requires "+
			"'go' in PATH (Kubernetes doesn't publish a server binary for %s/%s); install Go from https://go.dev/dl/",
			runtime.GOOS, runtime.GOARCH)
	}

	srcURL := fmt.Sprintf("%s/%s/kubernetes-src.tar.gz", dlBaseURL, version)
	progress(fmt.Sprintf("no official kube-controller-manager binary for %s/%s; building from official source "+
		"%s (this can take a minute)", runtime.GOOS, runtime.GOARCH, srcURL))

	tmpDir, err := os.MkdirTemp("", "kshrk-k8s-src-*")
	if err != nil {
		return fmt.Errorf("creating temp build directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tarPath := filepath.Join(tmpDir, "kubernetes-src.tar.gz")
	if err := downloadAndVerifyToFile(srcURL, tarPath); err != nil {
		return fmt.Errorf("downloading Kubernetes source: %w", err)
	}

	srcDir := filepath.Join(tmpDir, "src")
	if err := extractTarGz(tarPath, srcDir); err != nil {
		return fmt.Errorf("extracting Kubernetes source: %w", err)
	}

	vendorDir := filepath.Join(srcDir, "vendor")
	if info, err := os.Stat(vendorDir); err != nil || !info.IsDir() {
		return fmt.Errorf("kubernetes-src.tar.gz for %s has no vendor/ directory at %s; "+
			"cannot build without fetching dependencies from outside dl.k8s.io", version, vendorDir)
	}

	progress("compiling kube-controller-manager...")
	tmpBinPath, err := uniqueTempPath(filepath.Dir(destPath))
	if err != nil {
		return err
	}
	cmd := exec.Command(goBin, "build", "-mod=vendor", "-o", tmpBinPath, "./cmd/"+binName)
	cmd.Dir = srcDir
	// Force vendor-only module resolution: the Kubernetes source tarball ships
	// its full dependency tree under vendor/, and this package's "only fetches
	// from dl.k8s.io" guarantee depends on go build never reaching out to a
	// module proxy or VCS host for it. -mod=vendor alone would normally be
	// enough, but a GOFLAGS already set in the caller's environment could
	// override it, so GOFLAGS is scrubbed and re-set here, and GOPROXY=off
	// makes any resolution attempt that slips through fail loudly instead of
	// silently reaching the network.
	cmd.Env = withEnvOverride(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmpBinPath)
		return fmt.Errorf("go build ./cmd/%s: %w\n%s", binName, err, stderr.String())
	}
	return atomicInstall(tmpBinPath, destPath)
}

// withEnvOverride returns a copy of base with any existing entries for the
// same variable names as overrides removed, then overrides appended — so the
// override always wins regardless of duplicate-key lookup order, which
// varies across platforms/libc implementations.
func withEnvOverride(base []string, overrides ...string) []string {
	names := make(map[string]bool, len(overrides))
	for _, o := range overrides {
		if i := strings.IndexByte(o, '='); i >= 0 {
			names[o[:i]] = true
		}
	}
	env := make([]string, 0, len(base)+len(overrides))
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i >= 0 && names[e[:i]] {
			continue
		}
		env = append(env, e)
	}
	return append(env, overrides...)
}

// atomicInstall marks tmpPath executable and renames it into destPath. Go's
// os.Rename already replaces an existing destPath on every platform this
// package supports (Windows included: internal/syscall/windows.Rename passes
// MOVEFILE_REPLACE_EXISTING), so the common case is a single atomic,
// same-filesystem rename. The remove-then-retry fallback below only matters
// for edge cases plain rename can't handle regardless of platform (e.g.
// destPath somehow left behind as a directory from a prior version of this
// code, or an unwritable/locked leftover) — it isn't a full substitute for
// atomicity, just a best-effort recovery path.
func atomicInstall(tmpPath, destPath string) error {
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("marking binary executable: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		if rmErr := os.RemoveAll(destPath); rmErr == nil {
			if err := os.Rename(tmpPath, destPath); err == nil {
				return nil
			}
		}
		_ = os.Remove(tmpPath)
		return fmt.Errorf("installing binary: %w", err)
	}
	return nil
}

// downloadAndVerifyToFile downloads url to destPath, verifying the content
// against the SHA-256 checksum published at url+".sha256" (the convention
// dl.k8s.io uses for every release artifact). destPath is removed on any
// error, including a checksum mismatch, so a bad download never lingers
// where a later run's isExecutableFile check would find it.
func downloadAndVerifyToFile(url, destPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()

	wantSum, err := fetchChecksum(ctx, url+".sha256")
	if err != nil {
		return fmt.Errorf("fetching checksum: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := dlClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, hasher), resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(destPath)
		return fmt.Errorf("downloading %s: %w", url, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(destPath)
		return closeErr
	}

	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(gotSum, wantSum) {
		_ = os.Remove(destPath)
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, gotSum, wantSum)
	}
	return nil
}

// fetchChecksum fetches a dl.k8s.io ".sha256" companion file, which is a
// bare 64-character lowercase hex digest (optionally followed by whitespace
// and a filename, per the common sha256sum(1) format).
func fetchChecksum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := dlClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	sum := strings.TrimSpace(string(b))
	if i := strings.IndexAny(sum, " \t"); i >= 0 {
		sum = sum[:i]
	}
	if len(sum) != 64 {
		return "", fmt.Errorf("unexpected checksum format from %s: %q", url, sum)
	}
	return strings.ToLower(sum), nil
}

// maxExtractedBytes bounds total extracted size as a defense-in-depth guard
// against a corrupted or hostile archive, independent of the checksum check
// above (which only verifies the compressed tarball, not each entry as it's
// decompressed).
const maxExtractedBytes = 4 << 30 // 4 GiB

// extractTarGz extracts a gzip-compressed tar archive into destDir, which is
// created if needed. Entries are path-sanitized so nothing can escape
// destDir (no "..", no absolute paths), and only regular files and
// directories are extracted — symlinks and other special entry types are
// skipped rather than followed.
func extractTarGz(tarGzPath, destDir string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	cleanDestDir := filepath.Clean(destDir)

	tr := tar.NewReader(gz)
	var extracted int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		// hdr.Name comes from the archive and must never be trusted directly: a
		// hostile ".."-laden or absolute name could otherwise write outside
		// destDir ("Zip Slip"). filepath.Clean first normalizes harmless
		// internal ".." segments that still resolve inside destDir (e.g.
		// "foo/../bar" becomes "bar" — see TestExtractTarGz_PathTraversalRejected),
		// but any name that still escapes destDir after cleaning (a leading
		// "..", an absolute path) is rejected outright here — not remapped
		// into destDir — so an escaping entry never silently lands somewhere
		// surprising inside destDir instead of failing. filepath.Rel
		// re-verifies containment on the joined result as a second,
		// independent check before target is ever used in a filesystem
		// operation below.
		cleanedName := filepath.Clean(hdr.Name)
		if cleanedName == ".." || strings.HasPrefix(cleanedName, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanedName) {
			continue
		}
		target := filepath.Join(cleanDestDir, cleanedName)
		rel, relErr := filepath.Rel(cleanDestDir, target)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			// hdr.Size is attacker-controlled (it comes straight from the
			// archive); a malformed or hostile header could set it negative,
			// which would underflow the running extracted total and defeat
			// the maxExtractedBytes check below for every entry after it.
			if hdr.Size < 0 {
				return fmt.Errorf("tar entry %q has a negative size (%d); rejecting as malformed", cleanedName, hdr.Size)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode & 0o777)
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			extracted += hdr.Size
			if extracted > maxExtractedBytes {
				out.Close()
				return fmt.Errorf("archive exceeds %d bytes extracted; aborting", int64(maxExtractedBytes))
			}
			_, err = io.Copy(out, io.LimitReader(tr, hdr.Size))
			closeErr := out.Close()
			if err != nil {
				return fmt.Errorf("writing %s: %w", target, err)
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			// Skip symlinks and other special entry types.
			continue
		}
	}
}
