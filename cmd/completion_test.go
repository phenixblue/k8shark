package cmd

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCompletion drives Cobra's hidden completion command (the same entry point
// the generated shell scripts call) and returns the suggested completions plus
// the parsed ShellCompDirective.
func runCompletion(t *testing.T, args ...string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(append([]string{cobra.ShellCompRequestCmd}, args...))
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("completion request %v: %v", args, err)
	}

	var comps []string
	directive := cobra.ShellCompDirectiveError
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			n, err := strconv.Atoi(line[1:])
			if err != nil {
				t.Fatalf("completion request %v: bad directive line %q: %v", args, line, err)
			}
			directive = cobra.ShellCompDirective(n)
			continue
		}
		// Suggestions may carry a tab-separated description; keep the value.
		comps = append(comps, strings.SplitN(line, "\t", 2)[0])
	}
	return comps, directive
}

func TestCompleteArchiveArg(t *testing.T) {
	// No positional arg yet: offer *.kshrk files.
	comps, directive := completeArchiveArg(nil, nil, "")
	if want := []string{captureExt}; len(comps) != 1 || comps[0] != want[0] {
		t.Fatalf("comps = %v, want %v", comps, want)
	}
	if directive != cobra.ShellCompDirectiveFilterFileExt {
		t.Fatalf("directive = %d, want FilterFileExt (%d)", directive, cobra.ShellCompDirectiveFilterFileExt)
	}

	// Argument already supplied: nothing more to complete.
	comps, directive = completeArchiveArg(nil, []string{"capture.kshrk"}, "")
	if len(comps) != 0 {
		t.Fatalf("comps = %v, want none once the arg is filled", comps)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %d, want NoFileComp (%d)", directive, cobra.ShellCompDirectiveNoFileComp)
	}
}

func TestPositionalArchiveCompletionRegistered(t *testing.T) {
	for _, name := range []string{"inspect", "open", "ui", "transitions"} {
		comps, directive := runCompletion(t, name, "")
		if !contains(comps, captureExt) {
			t.Errorf("%s positional completion = %v, want it to include %q", name, comps, captureExt)
		}
		if directive&cobra.ShellCompDirectiveFilterFileExt == 0 {
			t.Errorf("%s directive = %d, want the FilterFileExt bit set", name, directive)
		}
	}
}

func TestOutputFlagEnumCompletion(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"inspect", []string{"table", "json", "yaml"}},
		{"transitions", []string{"table", "json"}},
		{"diff", []string{"text", "json"}},
	}
	for _, tc := range cases {
		comps, _ := runCompletion(t, tc.cmd, "--output", "")
		for _, w := range tc.want {
			if !contains(comps, w) {
				t.Errorf("%s --output completion = %v, want it to include %q", tc.cmd, comps, w)
			}
		}
	}
}

func TestArchiveFlagFilenameCompletion(t *testing.T) {
	// diff's --before/--after/--archive, redact's --in/--out, and capture's
	// --output are scoped to *.kshrk, which yields a file-extension-filter
	// directive.
	for _, c := range [][2]string{
		{"diff", "--before"},
		{"diff", "--after"},
		{"diff", "--archive"},
		{"redact", "--in"},
		{"redact", "--out"},
		{"capture", "--output"},
	} {
		comps, directive := runCompletion(t, c[0], c[1], "")
		if !contains(comps, captureExt) {
			t.Errorf("%s %s completion = %v, want %q", c[0], c[1], comps, captureExt)
		}
		if directive&cobra.ShellCompDirectiveFilterFileExt == 0 {
			t.Errorf("%s %s directive = %d, want the FilterFileExt bit set", c[0], c[1], directive)
		}
	}
}

func TestCaptureOutputCompletesStdout(t *testing.T) {
	// "-" (stream NDJSON to stdout) is still offered when the user starts
	// typing it, even though plain completion is scoped to *.kshrk files.
	// A dash-prefixed flag value is completed via the "--flag=value" form
	// (Cobra can't tell a space-separated "-" from the start of a new flag).
	comps, directive := runCompletion(t, "capture", "--output=-")
	if !contains(comps, "-") {
		t.Errorf("capture --output completion = %v, want it to include %q", comps, "-")
	}
	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Errorf("capture --output - directive = %d, want the NoFileComp bit set", directive)
	}
}

func TestConfigFlagYAMLCompletion(t *testing.T) {
	// The root persistent --config flag (used here via validate) and redact's
	// own --config flag are both scoped to YAML files.
	for _, name := range []string{"validate", "redact"} {
		comps, directive := runCompletion(t, name, "--config", "")
		for _, ext := range configExts {
			if !contains(comps, ext) {
				t.Errorf("%s --config completion = %v, want it to include %q", name, comps, ext)
			}
		}
		if directive&cobra.ShellCompDirectiveFilterFileExt == 0 {
			t.Errorf("%s --config directive = %d, want the FilterFileExt bit set", name, directive)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
