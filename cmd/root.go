package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "kshrk",
	Short: "k8shark — Kubernetes cluster state capture and replay",
	Long: `k8shark captures a Kubernetes cluster's state over time and replays
it through a mock API server, letting support engineers query a customer's
environment without direct connectivity.`,
	// SilenceErrors/SilenceUsage on the root command suppress cobra's own
	// "Error: ..." + usage-block printing for every subcommand (cobra checks
	// both the executing command's and the root's flag — see ExecuteC), so
	// Execute below is the single place that prints an error. Without this,
	// a command whose exitError carries an empty message (a clean --fail-on
	// gate trip) printed a bare "Error: " followed by a full usage dump, and
	// every other error printed twice (once by cobra, once here) (#217).
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Exit codes follow the diff(1)/git diff --exit-code convention, so CI can
// tell "the command ran and found something" apart from "the command
// couldn't run at all" (#217):
//
//	0 - clean: no findings/differences
//	1 - the command ran successfully and found something (a diagnose
//	    --fail-on gate trip, a diff with differences)
//	2 - the command failed (bad archive, invalid flags, I/O error, ...)
//
// A command signals exit code 1 by returning an exitError{code: exitCodeFindings};
// any other error (including a plain error from fmt.Errorf) falls through to
// the exit code 2 case below.
const (
	exitCodeFindings = 1
	exitCodeFailure  = 2
)

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Match specifically on the internal exitError type, not any error
		// implementing ExitCode() int — os/exec.ExitError also satisfies
		// that shape, and matching it here would let a failing subprocess
		// (kwok, kube-controller-manager, ...) escape with an arbitrary
		// exit code instead of the documented 0/1/2 contract.
		var exitErr exitError
		if errors.As(err, &exitErr) {
			if exitErr.msg != "" {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCodeFailure)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)")
	_ = rootCmd.MarkPersistentFlagFilename("config", configExts...)
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "enable verbose output")
	_ = viper.BindPFlag("verbose", rootCmd.PersistentFlags().Lookup("verbose"))
	// Read-side decryption flags. Deliberately NOT viper-bound: passphrase
	// material is read via os.Getenv / files only, never from config.yaml.
	addDecryptFlags(rootCmd)
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		// Search the current directory first, then the XDG-style
		// ~/.config/kshrk directory. Viper returns the first match in
		// the order the paths are added.
		viper.AddConfigPath(".")
		if home, err := os.UserHomeDir(); err == nil {
			viper.AddConfigPath(filepath.Join(home, ".config", "kshrk"))
		}
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
	}
	viper.AutomaticEnv()
	_ = viper.ReadInConfig()
}
