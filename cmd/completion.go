package cmd

import "github.com/spf13/cobra"

// captureExt is the file extension (without the leading dot) for k8shark
// capture archives. It scopes file completion to capture files for both
// positional archive arguments and archive-valued flags.
const captureExt = "kshrk"

// configExts are the file extensions (without the leading dot) for k8shark
// config files, used to scope completion of --config-style flags.
var configExts = []string{"yaml", "yml"}

// completeArchiveArg offers k8shark capture archives (*.kshrk) for a command's
// single positional archive argument, and nothing once that argument is filled.
// Cobra's default behavior would complete every file; this narrows it to the
// archives the command can actually open.
func completeArchiveArg(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []string{captureExt}, cobra.ShellCompDirectiveFilterFileExt
}
