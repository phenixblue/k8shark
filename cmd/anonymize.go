package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/anonymize"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
	"github.com/spf13/cobra"
)

var anonymizeCmd = &cobra.Command{
	Use:   "anonymize <capture.kshrk> --categories <category> [--out <anonymized.kshrk>]",
	Short: "Anonymize identifying values across a capture archive, consistently",
	Long: `Produces a new capture archive with values of the given categories replaced
by stable, deterministic aliases: the same original value maps to the same
alias everywhere it occurs in the archive, so relationships between objects
(a Pod's namespace, a Namespace's own identity, an Event's involvedObject)
stay intact. The original archive is not modified; the output defaults to
<in>-anonymized.kshrk.

This is a different tool than "kshrk redact": redact replaces one exact
field path with a fixed constant, everywhere it's configured to look;
anonymize replaces every occurrence of a value it recognizes, consistently,
using a deterministic alias derived from a salt.

Categories available: namespace, node, pod, workload, ip, url, image -- see
https://github.com/phenixblue/k8shark/issues/137.

Categories and field-path exclusion rules may also be loaded from a config
file's anonymize block via --config; see docs/config.md.`,
	Example: `  # Anonymize every namespace name in a capture
  kshrk anonymize capture.kshrk --categories namespace

  # Anonymize multiple categories in one pass
  kshrk anonymize capture.kshrk --categories namespace --categories node --categories pod

  # Anonymize IPs, hostnames, and image registries
  kshrk anonymize capture.kshrk --categories ip --categories url --categories image

  # Reproduce the exact same aliases on a re-run
  kshrk anonymize capture.kshrk --categories namespace --anonymize-salt-file salt.txt

  # Categories and exclusion rules from a config file, as JSON output
  kshrk anonymize capture.kshrk --config k8shark.yaml -o json

  # Emit the original-to-alias mapping, encrypted to a recipient
  kshrk anonymize capture.kshrk --categories namespace --emit-mapping --encrypt-recipient age1abc...

  # Anonymize and write to a chosen path
  kshrk anonymize capture.kshrk --out safe.kshrk --categories namespace`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runAnonymize,
}

func init() {
	rootCmd.AddCommand(anonymizeCmd)
	anonymizeCmd.Flags().String("out", "", "output archive path (default: <in>-anonymized.kshrk)")
	anonymizeCmd.Flags().StringArray("categories", nil, `category to anonymize (repeatable); supported: namespace, node, pod, workload, ip, url, image`)
	anonymizeCmd.Flags().StringP("output", "o", "text", "output format: text or json")
	anonymizeCmd.Flags().String("config", "", "capture config file whose anonymize.categories/anonymize.rules block is applied")
	anonymizeCmd.Flags().Bool("emit-mapping", false, "write the original-to-alias mapping alongside the output archive")
	anonymizeCmd.Flags().String("mapping-path", "", "path for the mapping file (default: <out>.mapping.json, or .mapping.json.age when encrypted)")
	anonymizeCmd.Flags().Bool("emit-mapping-plaintext", false, "allow --emit-mapping to write an unencrypted mapping when no --encrypt-* flag is set (not recommended)")
	_ = anonymizeCmd.MarkFlagFilename("out", captureExt)
	_ = anonymizeCmd.MarkFlagFilename("config", configExts...)
	_ = anonymizeCmd.RegisterFlagCompletionFunc("output",
		func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return []string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp
		})
	addAnonymizeFlags(anonymizeCmd)
	// Write-side encryption for the anonymized output (read-side --decrypt-*
	// are persistent flags from the root command, same as redact).
	addEncryptFlags(anonymizeCmd)
}

// supportedAnonymizeCategories must track internal/anonymize's own
// archiveCategories (archive.go) — kept as a separate list here (rather
// than exporting and reusing that one) because this is user-facing
// validation with its own error message, not the library's internal gate.
// internal/anonymize/archive_test.go's node/pod/workload integration tests
// are the real proof that Archive actually supports whatever this list
// claims; this list just needs to not get ahead of or behind that.
var supportedAnonymizeCategories = map[anonymize.Category]bool{
	anonymize.CategoryNamespace: true,
	anonymize.CategoryNode:      true,
	anonymize.CategoryPod:       true,
	anonymize.CategoryWorkload:  true,
	anonymize.CategoryIP:        true,
	anonymize.CategoryURL:       true,
	anonymize.CategoryImage:     true,
}

// parseAnonymizeCategories validates --categories against what this build's
// archive-rewrite path actually supports. Rejecting an unsupported category
// here, before any archive I/O, matches the discipline (*Aliaser).Alias
// enforces one layer down (alias.go) for the same reason: an unsupported
// category should fail loudly, not silently anonymize nothing for it.
func parseAnonymizeCategories(raw []string) ([]anonymize.Category, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf(`--categories is required (or anonymize.categories in a --config file); supported categories: namespace, node, pod, workload, ip, url, image (see #137)`)
	}
	out := make([]anonymize.Category, 0, len(raw))
	for _, c := range raw {
		cat := anonymize.Category(strings.ToLower(strings.TrimSpace(c)))
		if !supportedAnonymizeCategories[cat] {
			return nil, fmt.Errorf(`--categories %q: unsupported; supported categories: namespace, node, pod, workload, ip, url, image (see #137)`, c)
		}
		out = append(out, cat)
	}
	return out, nil
}

// dedupeCategories drops repeat entries while preserving first-seen order,
// so combining --categories flags with a config file's anonymize.categories
// (or simply repeating the same --categories value) doesn't produce a
// Categories slice with duplicates — harmless to Archive either way, but a
// clean, deduplicated list is what a --config-and-flags-combined -o json
// caller would reasonably expect to see echoed back.
func dedupeCategories(cats []anonymize.Category) []anonymize.Category {
	seen := make(map[anonymize.Category]bool, len(cats))
	out := make([]anonymize.Category, 0, len(cats))
	for _, c := range cats {
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

func runAnonymize(cmd *cobra.Command, args []string) error {
	in := args[0]
	out, _ := cmd.Flags().GetString("out")
	outputFormat, _ := cmd.Flags().GetString("output")
	categoryFlags, _ := cmd.Flags().GetStringArray("categories")
	cfgFile, _ := cmd.Flags().GetString("config")
	emitMapping, _ := cmd.Flags().GetBool("emit-mapping")
	mappingPath, _ := cmd.Flags().GetString("mapping-path")
	emitMappingPlaintext, _ := cmd.Flags().GetBool("emit-mapping-plaintext")

	if outputFormat != "text" && outputFormat != "json" {
		return fmt.Errorf("--output must be text or json (got %q)", outputFormat)
	}

	var rules []config.AnonymizeRule
	if cfgFile != "" {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		warnDeprecatedConfigKeys(cmd.ErrOrStderr(), cfg)
		categoryFlags = append(categoryFlags, cfg.Anonymize.Categories...)
		rules = append(rules, cfg.Anonymize.Rules...)
		if cfg.Anonymize.EmitMapping {
			emitMapping = true
		}
		if mappingPath == "" {
			mappingPath = cfg.Anonymize.MappingPath
		}
	}

	categories, err := parseAnonymizeCategories(categoryFlags)
	if err != nil {
		return err
	}
	categories = dedupeCategories(categories)

	if out == "" {
		// Trim either the current or the legacy extension so anonymizing a
		// "*.khsrk" capture yields "<in>-anonymized.kshrk", matching
		// redact's identical handling.
		base := strings.TrimSuffix(strings.TrimSuffix(in, ".kshrk"), ".khsrk")
		out = base + "-anonymized.kshrk"
	}

	// Refuse to overwrite the source.
	if err := rejectSamePath(in, out); err != nil {
		return err
	}

	salt, err := resolveAnonymizeSalt(cmd)
	if err != nil {
		return err
	}

	// Read-side: decrypt an encrypted source archive if a key was supplied.
	identities, err := resolveDecryptIdentities(cmd, in)
	if err != nil {
		return err
	}

	// Write-side: optionally encrypt the anonymized output (passphrase or
	// recipient keys).
	enc, err := resolveEncryption(cmd)
	if err != nil {
		return err
	}
	if !enc.enabled {
		if srcEncrypted, _ := archive.IsEncrypted(in); srcEncrypted {
			// The source is encrypted but no output encryption was
			// requested — warn that the anonymized copy will be written in
			// plaintext, matching redact's identical warning so an
			// encrypted capture isn't silently downgraded by either command.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: source archive is encrypted but the anonymized output %q will be written in plaintext; pass a passphrase (--encrypt / --encrypt-passphrase-file) or recipient (--encrypt-recipient / --encrypt-recipients-file) flag to keep it encrypted\n", out)
		}
	}

	// The mapping is the one genuinely sensitive artifact this command can
	// produce (see anonymize.Result.Mapping's own doc comment) — resolve
	// and validate everything about it *before* doing any archive I/O, so
	// a misconfigured --emit-mapping fails fast rather than after a
	// potentially expensive anonymize pass, or worse, after silently
	// truncating an archive (see the path-collision check below).
	if emitMapping && len(enc.recipients) == 0 && !emitMappingPlaintext {
		return fmt.Errorf("--emit-mapping would write an unencrypted mapping because no --encrypt-* flag is set; pass --encrypt / --encrypt-recipient (recommended) or --emit-mapping-plaintext to force plaintext output")
	}
	if emitMapping {
		if mappingPath == "" {
			mappingPath = out + ".mapping.json"
			if len(enc.recipients) > 0 {
				mappingPath += ".age"
			}
		}
		// writeAnonymizeMapping creates/truncates mappingPath — an explicit
		// --mapping-path (or a config file's mappingPath) that happens to
		// equal the source or destination archive would silently destroy
		// that archive the moment the mapping is written. The default path
		// above always differs from out (it appends a suffix), so this can
		// only fire for an explicit value. rejectSamePath's own error text
		// ("output path must differ from...") is written for the in/out
		// pair, so it's deliberately discarded here in favor of a message
		// that names --mapping-path specifically.
		if rejectSamePath(in, mappingPath) != nil {
			return fmt.Errorf("--mapping-path %q must not be the source archive", mappingPath)
		}
		if rejectSamePath(out, mappingPath) != nil {
			return fmt.Errorf("--mapping-path %q must not be the output archive", mappingPath)
		}
	}

	result, err := anonymize.Archive(in, out, anonymize.Options{
		Categories: categories,
		Salt:       salt,
		Identities: identities,
		Recipients: enc.recipients,
		Rules:      rules,
	})
	if err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	if fi != nil {
		result.OutputBytes = fi.Size()
	}
	result.OutputPath = out

	if emitMapping {
		if err := writeAnonymizeMapping(mappingPath, result.Mapping, enc.recipients); err != nil {
			return fmt.Errorf("writing mapping: %w", err)
		}
	}

	if outputFormat == "json" {
		jsonEnc := json.NewEncoder(cmd.OutOrStdout())
		jsonEnc.SetIndent("", "  ")
		return jsonEnc.Encode(result)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Anonymized %d namespace(s), %d node(s), %d pod(s), %d workload(s), %d IP(s), %d host(s), %d registry host(s) → %s (%d bytes)\n",
		result.NamespacesRenamed, result.NodesRenamed, result.PodsRenamed, result.WorkloadsRenamed,
		result.IPsRenamed, result.HostsRenamed, result.RegistriesRenamed, out, result.OutputBytes)
	if emitMapping {
		fmt.Fprintf(cmd.OutOrStdout(), "Mapping written to %s\n", mappingPath)
	}
	return nil
}

// writeAnonymizeMapping JSON-marshals mapping and writes it to path,
// age-encrypted to recipients when recipients is non-empty. The caller
// (runAnonymize) has already enforced that an empty recipients list here
// only happens when the user explicitly opted into plaintext output via
// --emit-mapping-plaintext.
func writeAnonymizeMapping(path string, mapping map[anonymize.Category]map[string]string, recipients []age.Recipient) error {
	data, err := json.MarshalIndent(mapping, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling mapping: %w", err)
	}

	// os.Create leaves permissions up to the process umask (commonly 0644,
	// world-readable) — this file can hold the original values behind
	// every alias, so it's created 0600 explicitly rather than trusting
	// whatever the umask happens to be, matching this codebase's existing
	// precedent for other sensitive file writes (internal/server/kubeconfig.go,
	// cmd/controllermanager.go).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating %q: %w", path, err)
	}
	defer f.Close()
	// OpenFile's mode argument only applies when the file is actually
	// created — if mappingPath already existed (e.g. a prior run left it
	// world-readable, before this 0600 discipline existed, or a re-run at
	// the same path), O_TRUNC alone leaves its existing permissions
	// untouched. Chmod unconditionally to guarantee 0600 either way.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", path, err)
	}

	if len(recipients) == 0 {
		_, err := f.Write(data)
		return err
	}

	w, err := age.Encrypt(f, recipients...)
	if err != nil {
		return fmt.Errorf("encrypting mapping: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing encrypted mapping: %w", err)
	}
	return w.Close()
}
