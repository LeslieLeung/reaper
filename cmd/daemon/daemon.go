package daemon

import (
	"github.com/go-co-op/gocron"
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/rip"
	"github.com/leslieleung/reaper/internal/typedef"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/spf13/cobra"
	"time"
)

var Cmd = &cobra.Command{
	Use:   "daemon",
	Short: "daemon runs as a daemon to monitor git repositories",
	Run:   runDaemon,
}

func runDaemon(cmd *cobra.Command, args []string) {
	storageMap := config.GetStorageMap()

	s := gocron.NewScheduler(time.Local)
	s.SetMaxConcurrentJobs(3, gocron.WaitMode)

	for _, repo := range rip.GetRepositories("") {
		if repo.Cron == "" {
			continue
		}
		storages := make([]typedef.MultiStorage, 0)
		for _, storage := range repo.Storage {
			if s, ok := storageMap[storage]; !ok {
				continue
			} else {
				storages = append(storages, s)
			}
		}
		_, err := s.Cron(repo.Cron).Do(rip.Rip, repo, storages)
		if err != nil {
			ui.Errorf("Error scheduling %s, %s", repo.Name, err)
		}
		ui.Printf("Scheduled %s, cron: %s", repo.Name, repo.Cron)
	}
	ui.Printf("Starting daemon")
	s.StartBlocking()
}
