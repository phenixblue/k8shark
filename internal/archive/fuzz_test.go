package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzOpenArchive guards the untrusted-input surface #254 calls out:
// .kshrk files move between organizations, so Open must never panic or
// allocate without bound on arbitrary bytes, only ever return an error.
// Seeded with the golden archives (plain and passphrase-encrypted, across
// both format versions) so the fuzzer starts from real zip/age structure
// and mutates from there rather than starting from nothing.
func FuzzOpenArchive(f *testing.F) {
	for _, seed := range []string{
		"testdata/golden-v1.kshrk",
		"testdata/golden-v1-passphrase.kshrk",
		"testdata/golden-v2.kshrk",
		"testdata/golden-v2-passphrase.kshrk",
	} {
		data, err := os.ReadFile(seed)
		if err != nil {
			f.Fatalf("reading seed %s: %v", seed, err)
		}
		f.Add(data)
	}
	f.Add([]byte{})
	f.Add([]byte("not a zip file"))

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.kshrk")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("writing fuzz input: %v", err)
		}
		ar, err := Open(path)
		if err != nil {
			return // any error is a valid outcome for untrusted input
		}
		defer ar.Close()
		_, _ = ar.ReadMetadata()
	})
}
