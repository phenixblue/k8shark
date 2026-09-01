package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/phenixblue/k8shark/internal/anonymize"
	"github.com/phenixblue/k8shark/internal/archive"
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

Categories available so far: namespace, node, pod, workload. More land
milestone by milestone -- see https://github.com/phenixblue/k8shark/issues/137.`,
	Example: `  # Anonymize every namespace name in a capture
  kshrk anonymize capture.kshrk --categories namespace

  # Anonymize multiple categories in one pass
  kshrk anonymize capture.kshrk --categories namespace --categories node --categories pod

  # Reproduce the exact same aliases on a re-run
  kshrk anonymize capture.kshrk --categories namespace --anonymize-salt-file salt.txt

  # Anonymize and write to a chosen path
  kshrk anonymize capture.kshrk --out safe.kshrk --categories namespace`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runAnonymize,
}

func init() {
	rootCmd.AddCommand(anonymizeCmd)
	anonymizeCmd.Flags().String("out", "", "output archive path (default: <in>-anonymized.kshrk)")
	anonymizeCmd.Flags().StringArray("categories", nil, `category to anonymize (repeatable); supported: namespace, node, pod, workload`)
	_ = anonymizeCmd.MarkFlagFilename("out", captureExt)
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
}

// parseAnonymizeCategories validates --categories against what this build's
// archive-rewrite path actually supports. Rejecting an unsupported category
// here, before any archive I/O, matches the discipline (*Aliaser).Alias
// enforces one layer down (alias.go) for the same reason: an unsupported
// category should fail loudly, not silently anonymize nothing for it.
func parseAnonymizeCategories(raw []string) ([]anonymize.Category, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf(`--categories is required; supported categories: namespace, node, pod, workload (see #137)`)
	}
	out := make([]anonymize.Category, 0, len(raw))
	for _, c := range raw {
		cat := anonymize.Category(strings.ToLower(strings.TrimSpace(c)))
		if !supportedAnonymizeCategories[cat] {
			return nil, fmt.Errorf(`--categories %q: not yet supported; supported categories: namespace, node, pod, workload (see #137)`, c)
		}
		out = append(out, cat)
	}
	return out, nil
}

func runAnonymize(cmd *cobra.Command, args []string) error {
	in := args[0]
	out, _ := cmd.Flags().GetString("out")
	categoryFlags, _ := cmd.Flags().GetStringArray("categories")

	categories, err := parseAnonymizeCategories(categoryFlags)
	if err != nil {
		return err
	}

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

	result, err := anonymize.Archive(in, out, anonymize.Options{
		Categories: categories,
		Salt:       salt,
		Identities: identities,
		Recipients: enc.recipients,
	})
	if err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}

	fmt.Printf("Anonymized %d namespace(s), %d node(s), %d pod(s), %d workload(s) → %s (%d bytes)\n",
		result.NamespacesRenamed, result.NodesRenamed, result.PodsRenamed, result.WorkloadsRenamed, out, size)
	return nil
}
