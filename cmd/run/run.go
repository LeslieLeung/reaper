package run

import (
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/rip"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "run",
	Short: "run runs all repositories defined in config",
	Run:   runRun,
}

func runRun(cmd *cobra.Command, args []string) {
	storageMap := config.GetStorageMap()

	for _, repo := range config.GetRepositories() {
		storages := make([]config.MultiStorage, 0)
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
