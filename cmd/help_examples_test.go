package cmd

import "testing"

// Every user-facing subcommand should ship copy-pasteable examples so
// `kshrk <cmd> --help` renders an "Examples:" section. version/help/completion
// are exempt.
func TestUserFacingCommandsHaveExamples(t *testing.T) {
	exempt := map[string]bool{"version": true, "help": true, "completion": true}
	for _, c := range rootCmd.Commands() {
		// Hidden commands (e.g. Cobra's "__complete" helper, added once
		// completion is requested) aren't user-facing.
		if c.Hidden || exempt[c.Name()] {
			continue
		}
		if c.Example == "" {
			t.Errorf("command %q has no Example (add a Cobra Example field)", c.Name())
		}
	}
}
