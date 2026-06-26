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
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) {
			if err.Error() != "" {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml, then ~/.config/kshrk/config.yaml)")
	_ = rootCmd.MarkPersistentFlagFilename("config", configExts...)
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "enable verbose output")
	_ = viper.BindPFlag("verbose", rootCmd.PersistentFlags().Lookup("verbose"))
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
