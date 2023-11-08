package rip

import (
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/rip"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/spf13/cobra"
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
	storages := make([]config.MultiStorage, 0)
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
	for _, storage := range repo.Storage {
		for _, s := range cfg.Storage {
			if s.Name == storage {
				storages = append(storages, s)
			}
		}
	}
	if len(storages) != len(repo.Storage) {
		ui.ErrorfExit("Storage missing in config")
	}

	if err := rip.Rip(repo, storages); err != nil {
		ui.ErrorfExit("Error running %s, %s", repo.Name, err)
	}
}
