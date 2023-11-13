package rip

import (
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/rip"
	"github.com/leslieleung/reaper/internal/typedef"
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
	storageMap := config.GetStorageMap()
	// find repo in config
	for _, repo := range rip.GetRepositories(repoName) {
		storages := make([]typedef.MultiStorage, 0)
		for _, storage := range repo.Storage {
			if s, ok := storageMap[storage]; !ok {
				ui.Errorf("Storage %s not found in config", storage)
				continue
			} else {
				storages = append(storages, s)
			}
		}
		ui.Printf("Running %s", repo.Name)
		if err := rip.Rip(repo, storages); err != nil {
			ui.Errorf("Error running %s, %s", repo.Name, err)
			// move on to next repo
		}
	}
}
