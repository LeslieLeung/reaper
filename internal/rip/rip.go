package rip

import (
	"context"
	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"
	"github.com/leslieleung/reaper/internal/config"
	"github.com/leslieleung/reaper/internal/storage"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/mholt/archiver/v4"
	"os"
	"path"

	"time"
)

func Rip(repo config.Repository, storages []config.Storage) {
	id := uuid.New().String()
	// get current directory
	currentDir, _ := os.Getwd()
	// create a working directory
	err := os.MkdirAll(path.Join(currentDir, ".reaper", id), 0774)
	if err != nil {
		ui.ErrorfExit("Error creating working directory, %s", err)
	}
	err = os.Chmod(path.Join(currentDir, ".reaper", id), 0774)
	if err != nil {
		ui.ErrorfExit("Error changing permission of working directory, %s", err)
	}

	// clone the repo
	_, err = git.PlainClone(path.Join(currentDir, ".reaper", id), false, &git.CloneOptions{
		URL:      "https://" + repo.URL,
		Progress: os.Stdout,
	})

	if err != nil {
		ui.ErrorfExit("Error cloning repository, %s", err)
	}

	ui.Printf("Repository %s cloned", repo.Name)
	files, err := archiver.FilesFromDisk(nil, map[string]string{
		path.Join(currentDir, ".reaper", id): repo.Name,
		// path.Join(currentDir, ".reaper", id): "repo.Name",
		// TODO add file hash
	})
	if err != nil {
		ui.ErrorfExit("Error reading files, %s", err)
	}

	now := time.Now().Format("20060102150405")
	base := repo.Name + "-" + now + ".tar.gz"
	p := path.Join(currentDir, ".reaper", base)
	out, err := os.Create(p)
	if err != nil {
		ui.ErrorfExit("Error creating archive, %s", err)
	}
	defer out.Close()
	format := archiver.CompressedArchive{
		Compression: archiver.Gz{},
		Archival:    archiver.Tar{},
	}
	err = format.Archive(context.Background(), out, files)
	if err != nil {
		ui.ErrorfExit("Error creating archive, %s", err)
	}

	// handle storages
	for _, s := range storages {
		switch s.Type {
		case "file":
			fileBackend := storage.File{}
			err := fileBackend.PutObjectFromPath(p, path.Join(s.Path, base))
			if err != nil {
				ui.ErrorfExit("Error storing file, %s", err)
			}
			ui.Printf("File %s stored", path.Join(s.Path, base))
		}
	}

	// cleanup
	err = os.RemoveAll(path.Join(currentDir, ".reaper", id))
	if err != nil {
		ui.ErrorfExit("Error cleaning up working directory, %s", err)
	}
	err = os.Remove(p)
	if err != nil {
		ui.ErrorfExit("Error cleaning up archive, %s", err)
	}
}
