package rip

import (
	"bytes"
	"context"
	"errors"
	"github.com/go-git/go-git/v5"
	"github.com/google/uuid"
	"github.com/leslieleung/reaper/internal/storage"
	"github.com/leslieleung/reaper/internal/typedef"
	"github.com/leslieleung/reaper/internal/ui"
	"github.com/mholt/archiver/v4"
	"os"
	"path"

	"time"
)

func Rip(repo typedef.Repository, storages []typedef.MultiStorage) error {
	useCache := repo.UseCache
	// get current directory
	currentDir, _ := os.Getwd()

	var workingDir string
	if useCache {
		workingDir = path.Join(currentDir, ".reaper")
	} else {
		id := uuid.New().String()
		workingDir = path.Join(currentDir, ".reaper", id)
	}

	// create a working directory if not exist
	err := storage.CreateDirIfNotExist(workingDir)
	if err != nil {
		ui.Errorf("Error creating working directory, %s", err)
		return err
	}

	// get the repo name from the URL
	repoName := path.Base(repo.URL)
	// check if repo name is valid
	if repoName == "." || repo.Name == "/" {
		ui.Errorf("Invalid repository name")
		return err
	}
	gitDir := path.Join(workingDir, repoName)
	var exist bool
	// check if the repo already exists
	if _, err := os.Stat(path.Join(gitDir, ".git")); err == nil {
		exist = true
	}
	// clone the repo if it does not exist, otherwise pull
	if !exist {
		_, err = git.PlainClone(gitDir, false, &git.CloneOptions{
			URL:      "https://" + repo.URL,
			Progress: os.Stdout,
		})

		if err != nil {
			ui.Errorf("Error cloning repository, %s", err)
			return err
		}

		ui.Printf("Repository %s cloned", repo.Name)
	} else {
		r, err := git.PlainOpen(gitDir)
		if err != nil {
			ui.Errorf("Error opening repository, %s", err)
			return err
		}
		w, err := r.Worktree()
		if err != nil {
			ui.Errorf("Error getting worktree, %s", err)
			return err
		}
		err = w.Pull(&git.PullOptions{
			RemoteName: "origin",
			Progress:   os.Stdout,
		})
		if err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				ui.Printf("Repository %s already up to date", repo.Name)
				return nil
			}
			ui.Errorf("Error pulling repository, %s", err)
			return err
		}
		ui.Printf("Repository %s pulled", repo.Name)
	}

	files, err := archiver.FilesFromDisk(nil, map[string]string{
		workingDir: repo.Name,
	})
	if err != nil {
		ui.Errorf("Error reading files, %s", err)
		return err
	}

	now := time.Now().Format("20060102150405")
	base := repo.Name + "-" + now + ".tar.gz"
	// TODO store to a temporary file first if greater than certain size
	archive := &bytes.Buffer{}

	format := archiver.CompressedArchive{
		Compression: archiver.Gz{},
		Archival:    archiver.Tar{},
	}
	err = format.Archive(context.Background(), archive, files)
	if err != nil {
		ui.Errorf("Error creating archive, %s", err)
		return err
	}

	// handle storages
	for _, s := range storages {
		var err error
		switch s.Type {
		case storage.FileStorage:
			fileBackend := storage.File{}
			err = fileBackend.PutObject(path.Join(s.Path, base), archive.Bytes())
		case storage.S3Storage:
			s3Backend, err := storage.New(s.Endpoint, s.Bucket, s.Region, s.AccessKeyID, s.SecretAccessKey)
			if err != nil {
				ui.Errorf("Error creating S3 backend, %s", err)
				return err
			}
			err = s3Backend.PutObject(base, archive.Bytes())
		}
		if err != nil {
			ui.Errorf("Error storing file, %s", err)
			return err
		}
		ui.Printf("File %s stored", path.Join(s.Path, base))
	}

	// cleanup
	if !useCache {
		err = os.RemoveAll(workingDir)
		if err != nil {
			ui.Errorf("Error cleaning up working directory, %s", err)
			return err
		}
	}
	return nil
}
