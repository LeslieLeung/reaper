package cmd

import (
	"fmt"
	"github.com/spf13/cobra"
	"reaper/cmd/rip"
	"reaper/cmd/run"
	"reaper/internal/config"
	"reaper/internal/ui"
)

var rootCmd = &cobra.Command{
	Use:   "reaper",
	Short: "Reaper is a tool to backup git repositories.",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		ui.ErrorfExit(fmt.Sprintf("Error executing command, %s", err))
	}
}

func init() {
	cobra.OnInitialize(config.Init)
	// commands
	rootCmd.AddCommand(rip.Cmd)
	rootCmd.AddCommand(run.Cmd)
	// flags
	rootCmd.PersistentFlags().StringVarP(&config.Path, "config", "c", "config.yaml", "config file path")
}
