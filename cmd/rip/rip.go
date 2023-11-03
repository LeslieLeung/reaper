package rip

import (
	"github.com/spf13/cobra"
	"reaper/internal/config"
	"reaper/internal/rip"
	"reaper/internal/ui"
)

var Cmd = &cobra.Command{
	Use:   "rip",
	Short: "rip immediately runs a job",
	Run:   runRip,
	Args:  cobra.ExactArgs(1),
}

func runRip(cmd *cobra.Command, args []string) {
	repoName := args[0]
	// find repo in config
	cfg := config.GetIns()
	var repo config.Repository
	storages := make([]config.Storage, 0)
	var found bool
	for _, repository := range cfg.Repository {
		if repository.Name == repoName {
			repo = repository
			found = true
			break
		}
	}
	if !found {
		ui.ErrorfExit("Repository %s not found in config", repoName)
	}
	if repo.URL == "" {
		ui.ErrorfExit("Repository %s has no URL", repoName)
	}
	found = false
	for _, storage := range repo.Storage {
		for _, s := range cfg.Storage {
			if s.Name == storage {
				storages = append(storages, s)
				found = true
				break
			}
		}
	}
	if !found {
		ui.ErrorfExit("Storage %s not found in config", repo.Storage)
	}

	rip.Rip(repo, storages)
}
