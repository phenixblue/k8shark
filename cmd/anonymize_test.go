package cmd

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/anonymize"
	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/spf13/cobra"
)

func TestParseAnonymizeCategories(t *testing.T) {
	t.Run("namespace is accepted", func(t *testing.T) {
		got, err := parseAnonymizeCategories([]string{"namespace"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != anonymize.CategoryNamespace {
			t.Errorf("got %v, want [namespace]", got)
		}
	})

	t.Run("case and whitespace are normalized", func(t *testing.T) {
		got, err := parseAnonymizeCategories([]string{"  Namespace  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != anonymize.CategoryNamespace {
			t.Errorf("got %v, want [namespace]", got)
		}
	})

	t.Run("empty is rejected — --categories is required", func(t *testing.T) {
		if _, err := parseAnonymizeCategories(nil); err == nil {
			t.Error("want an error when no categories are given")
		}
	})

	t.Run("node, pod, and workload are accepted", func(t *testing.T) {
		for cat, want := range map[string]anonymize.Category{
			"node":     anonymize.CategoryNode,
			"pod":      anonymize.CategoryPod,
			"workload": anonymize.CategoryWorkload,
		} {
			got, err := parseAnonymizeCategories([]string{cat})
			if err != nil {
				t.Errorf("category %q: unexpected error: %v", cat, err)
				continue
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("category %q: got %v, want [%v]", cat, got, want)
			}
		}
	})

	t.Run("ip, url, and image are accepted", func(t *testing.T) {
		for cat, want := range map[string]anonymize.Category{
			"ip":    anonymize.CategoryIP,
			"url":   anonymize.CategoryURL,
			"image": anonymize.CategoryImage,
		} {
			got, err := parseAnonymizeCategories([]string{cat})
			if err != nil {
				t.Errorf("category %q: unexpected error: %v", cat, err)
				continue
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("category %q: got %v, want [%v]", cat, got, want)
			}
		}
	})

	t.Run("multiple supported categories in one call are all accepted", func(t *testing.T) {
		got, err := parseAnonymizeCategories([]string{"namespace", "node", "pod", "workload", "ip", "url", "image"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 7 {
			t.Errorf("got %v, want 7 categories", got)
		}
	})

	// As of M4 (#137), every real Category constant is supported by the
	// archive-rewrite path — there's no longer a real category name left to
	// exercise this with. A made-up category string still needs to be
	// rejected, though.
	t.Run("a made-up category is rejected", func(t *testing.T) {
		if _, err := parseAnonymizeCategories([]string{"bogus"}); err == nil {
			t.Error("want an error for a made-up category")
		}
	})

	t.Run("one bad category in a list rejects the whole list", func(t *testing.T) {
		if _, err := parseAnonymizeCategories([]string{"namespace", "bogus"}); err == nil {
			t.Error("want an error when any requested category is unsupported")
		}
	})
}

func TestDedupeCategories(t *testing.T) {
	got := dedupeCategories([]anonymize.Category{
		anonymize.CategoryNamespace, anonymize.CategoryNode, anonymize.CategoryNamespace,
	})
	want := []anonymize.Category{anonymize.CategoryNamespace, anonymize.CategoryNode}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %v, want %v (order must be first-seen)", i, got[i], want[i])
		}
	}
}

// newTestAnonymizeCmdCommand mirrors newTestDecryptCmdCommand's pattern
// (decrypt_test.go): a standalone command carrying every flag runAnonymize
// reads, so tests can call runAnonymize directly instead of going through
// cobra's full command tree / Execute().
func newTestAnonymizeCmdCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.Flags().String("out", "", "")
	cmd.Flags().StringArray("categories", nil, "")
	cmd.Flags().StringP("output", "o", "text", "")
	cmd.Flags().String("config", "", "")
	cmd.Flags().Bool("emit-mapping", false, "")
	cmd.Flags().String("mapping-path", "", "")
	cmd.Flags().Bool("emit-mapping-plaintext", false, "")
	addAnonymizeFlags(cmd)
	addEncryptFlags(cmd)
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	return cmd
}

// anonymizeTestSalt is a fixed, valid hex salt used across these tests so
// runAnonymize doesn't hit resolveAnonymizeSalt's generate-and-warn path,
// which would print to stderr and make aliases non-reproducible between
// assertions within one test.
var anonymizeTestSalt = hex.EncodeToString([]byte("anonymize-cmd-test-salt-32-bytes"))

func writeAnonymizeTestSaltFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "salt.txt")
	if err := os.WriteFile(path, []byte(anonymizeTestSalt), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunAnonymize_ConfigFileSuppliesCategories(t *testing.T) {
	in := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"prod"}}]}`)
	cfgPath := filepath.Join(t.TempDir(), "k8shark.yaml")
	if err := os.WriteFile(cfgPath, []byte("anonymize:\n  categories: [namespace]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTestAnonymizeCmdCommand(t)
	cmd.SetOut(io.Discard)
	_ = cmd.Flags().Set("config", cfgPath)
	_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

	// No --categories flag at all: the config file must supply it on its own.
	if err := runAnonymize(cmd, []string{in}); err != nil {
		t.Fatalf("runAnonymize: %v", err)
	}

	wantOut := strings.TrimSuffix(in, ".kshrk") + "-anonymized.kshrk"
	if fi, err := os.Stat(wantOut); err != nil || fi.Size() == 0 {
		t.Fatalf("default output %q not created (or empty): %v", wantOut, err)
	}
}

func TestRunAnonymize_ConfigFileRuleExcludesFieldPath(t *testing.T) {
	// "default" matches buildDiffArchive's own hardcoded APIPath namespace
	// segment, so there is exactly one distinct namespace value in play —
	// the path itself still gets renamed regardless of the rule (rules
	// gate only the body's own field-write site, not the path rewrite),
	// but the body's own metadata.namespace must survive unchanged.
	in := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"default"}}]}`)
	cfgPath := filepath.Join(t.TempDir(), "k8shark.yaml")
	cfgYAML := "anonymize:\n" +
		"  categories: [namespace]\n" +
		"  rules:\n" +
		"    - category: namespace\n" +
		"      kind: Pod\n" +
		"      fieldPath: metadata.namespace\n" +
		"      exclude: true\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.kshrk")

	cmd := newTestAnonymizeCmdCommand(t)
	cmd.SetOut(io.Discard)
	_ = cmd.Flags().Set("config", cfgPath)
	_ = cmd.Flags().Set("out", out)
	_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

	if err := runAnonymize(cmd, []string{in}); err != nil {
		t.Fatalf("runAnonymize: %v", err)
	}

	ar, err := archivepkg.Open(out)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()
	idx, err := ar.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(idx) != 1 {
		t.Fatalf("index has %d entries, want 1: %v", len(idx), idx)
	}
	var onlyPath string
	for p := range idx {
		onlyPath = p
	}
	data, err := ar.ReadRecord(onlyPath, 0)
	if err != nil {
		t.Fatalf("ReadRecord(%q, 0): %v", onlyPath, err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var podList map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &podList); err != nil {
		t.Fatal(err)
	}
	gotNS := podList["items"].([]interface{})[0].(map[string]interface{})["metadata"].(map[string]interface{})["namespace"]
	if gotNS != "default" {
		t.Errorf("Pod metadata.namespace = %v, want unchanged \"default\" — excluded by the config-supplied rule", gotNS)
	}
}

func TestRunAnonymize_JSONOutput(t *testing.T) {
	// "default" matches buildDiffArchive's own hardcoded APIPath namespace
	// segment, so there is exactly one distinct namespace value to count.
	in := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"default"}}]}`)

	cmd := newTestAnonymizeCmdCommand(t)
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	_ = cmd.Flags().Set("categories", "namespace")
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

	if err := runAnonymize(cmd, []string{in}); err != nil {
		t.Fatalf("runAnonymize: %v", err)
	}

	var result anonymize.Result
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\noutput: %s", err, stdout.String())
	}
	if result.SchemaVersion != anonymize.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", result.SchemaVersion, anonymize.SchemaVersion)
	}
	if result.NamespacesRenamed != 1 {
		t.Errorf("namespaces_renamed = %d, want 1", result.NamespacesRenamed)
	}
	if result.OutputPath == "" {
		t.Error("output_path is empty")
	}

	// Decode the top-level key set directly, rather than substring-matching
	// the Go field name "Mapping" — that would miss a regression where a
	// future retag exposed it under a differently-cased or differently-named
	// key (e.g. "mapping"). Every legitimate key is checked for presence and
	// every present key is checked for not looking like the mapping, so
	// renaming a real field wouldn't silently pass this either.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout.String()), &raw); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\noutput: %s", err, stdout.String())
	}
	for _, k := range []string{
		"schema_version", "namespaces_renamed", "nodes_renamed", "pods_renamed",
		"workloads_renamed", "ips_renamed", "hosts_renamed", "registries_renamed", "output_path",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing expected top-level key %q; got %s", k, stdout.String())
		}
	}
	for k := range raw {
		if strings.Contains(strings.ToLower(k), "mapping") {
			t.Errorf("-o json output must never contain the mapping under any key; found key %q: %s", k, stdout.String())
		}
	}
}

func TestRunAnonymize_EmitMappingRequiresEncryptionOrAck(t *testing.T) {
	in := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"prod"}}]}`)

	t.Run("rejected with neither encryption nor the plaintext ack", func(t *testing.T) {
		cmd := newTestAnonymizeCmdCommand(t)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("categories", "namespace")
		_ = cmd.Flags().Set("out", filepath.Join(t.TempDir(), "out.kshrk"))
		_ = cmd.Flags().Set("emit-mapping", "true")
		_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

		if err := runAnonymize(cmd, []string{in}); err == nil {
			t.Fatal("want an error when --emit-mapping is set with no encryption and no --emit-mapping-plaintext")
		}
	})

	t.Run("plaintext ack writes a readable JSON mapping", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "out.kshrk")
		cmd := newTestAnonymizeCmdCommand(t)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("categories", "namespace")
		_ = cmd.Flags().Set("out", out)
		_ = cmd.Flags().Set("emit-mapping", "true")
		_ = cmd.Flags().Set("emit-mapping-plaintext", "true")
		_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

		if err := runAnonymize(cmd, []string{in}); err != nil {
			t.Fatalf("runAnonymize: %v", err)
		}

		mappingPath := out + ".mapping.json"
		data, err := os.ReadFile(mappingPath)
		if err != nil {
			t.Fatalf("reading mapping file: %v", err)
		}
		var mapping map[string]map[string]string
		if err := json.Unmarshal(data, &mapping); err != nil {
			t.Fatalf("mapping file is not valid JSON: %v", err)
		}
		if mapping["namespace"]["prod"] == "" {
			t.Errorf("mapping[namespace][prod] is empty; mapping = %v", mapping)
		}

		// The mapping holds every original value behind an alias — it must
		// never be created at the process umask's default (commonly 0644,
		// world-readable), regardless of platform umask settings. Unix
		// permission bits don't apply on Windows (mirrors
		// internal/archive/crypto_test.go's identical skip for the same
		// reason).
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(mappingPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("mapping file mode = %o, want 0600", got)
			}
		}
	})

	t.Run("an encryption recipient satisfies the requirement and encrypts the mapping", func(t *testing.T) {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "out.kshrk")
		cmd := newTestAnonymizeCmdCommand(t)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("categories", "namespace")
		_ = cmd.Flags().Set("out", out)
		_ = cmd.Flags().Set("emit-mapping", "true")
		_ = cmd.Flags().Set("encrypt-recipient", id.Recipient().String())
		_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

		if err := runAnonymize(cmd, []string{in}); err != nil {
			t.Fatalf("runAnonymize: %v", err)
		}

		mappingPath := out + ".mapping.json.age"
		f, err := os.Open(mappingPath)
		if err != nil {
			t.Fatalf("opening encrypted mapping file: %v", err)
		}
		defer f.Close()
		r, err := age.Decrypt(f, id)
		if err != nil {
			t.Fatalf("age.Decrypt: %v", err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("reading decrypted mapping: %v", err)
		}
		var mapping map[string]map[string]string
		if err := json.Unmarshal(data, &mapping); err != nil {
			t.Fatalf("decrypted mapping is not valid JSON: %v", err)
		}
		if mapping["namespace"]["prod"] == "" {
			t.Errorf("mapping[namespace][prod] is empty; mapping = %v", mapping)
		}
	})
}

// --mapping-path colliding with the source or destination archive would
// have writeAnonymizeMapping's create/truncate silently destroy that
// archive — this must be rejected before any archive I/O runs at all, not
// discovered after the anonymize pass already succeeded.
func TestRunAnonymize_EmitMappingRejectsPathCollisions(t *testing.T) {
	// Each subtest builds its own source archive, deliberately not shared:
	// if the guard under test ever regressed, the first subtest would
	// actually corrupt a shared "in" by writing the mapping over it, and
	// the second subtest would then "pass" for the wrong reason (Archive
	// failing to open the now-corrupted source) rather than because its
	// own guard fired.
	t.Run("--mapping-path equal to the source archive is rejected", func(t *testing.T) {
		in := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"prod"}}]}`)
		out := filepath.Join(t.TempDir(), "out.kshrk")
		cmd := newTestAnonymizeCmdCommand(t)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("categories", "namespace")
		_ = cmd.Flags().Set("out", out)
		_ = cmd.Flags().Set("emit-mapping", "true")
		_ = cmd.Flags().Set("emit-mapping-plaintext", "true")
		_ = cmd.Flags().Set("mapping-path", in)
		_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

		if err := runAnonymize(cmd, []string{in}); err == nil {
			t.Fatal("want an error when --mapping-path equals the source archive")
		}
		// And the source archive itself must survive untouched — the whole
		// point of catching this before any archive I/O.
		fi, statErr := os.Stat(in)
		if statErr != nil || fi.Size() == 0 {
			t.Errorf("source archive %q was damaged (or removed): stat err=%v, size=%v", in, statErr, fi)
		}
	})

	t.Run("--mapping-path equal to the output archive is rejected", func(t *testing.T) {
		in := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"web-1","namespace":"prod"}}]}`)
		out := filepath.Join(t.TempDir(), "out.kshrk")
		cmd := newTestAnonymizeCmdCommand(t)
		cmd.SetOut(io.Discard)
		_ = cmd.Flags().Set("categories", "namespace")
		_ = cmd.Flags().Set("out", out)
		_ = cmd.Flags().Set("emit-mapping", "true")
		_ = cmd.Flags().Set("emit-mapping-plaintext", "true")
		_ = cmd.Flags().Set("mapping-path", out)
		_ = cmd.Flags().Set("anonymize-salt-file", writeAnonymizeTestSaltFile(t))

		if err := runAnonymize(cmd, []string{in}); err == nil {
			t.Fatal("want an error when --mapping-path equals the output archive")
		}
		// The whole point: caught before Archive() ever runs, so no output
		// file should exist at all — not a truncated one.
		if _, statErr := os.Stat(out); statErr == nil {
			t.Error("output archive should not have been written at all; the collision must be caught before any archive I/O")
		}
	})
}
