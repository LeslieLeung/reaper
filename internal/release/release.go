package release

import (
	"fmt"
	"github.com/leslieleung/reaper/internal/scm"
	"github.com/leslieleung/reaper/internal/scm/github"
	"github.com/leslieleung/reaper/internal/storage"
	"github.com/leslieleung/reaper/internal/typedef"
	"github.com/leslieleung/reaper/internal/ui"
	"io"
)

func DownloadAllAssets(repo typedef.Repository, storages []typedef.MultiStorage) error {
	r, err := scm.NewRepository(repo.URL)
	if err != nil {
		return err
	}
	c, err := github.New()
	if err != nil {
		return err
	}
	// get all releases
	releases, err := c.GetReleases(r.Owner, r.Name)
	if err != nil {
		return err
	}
	for _, release := range releases {
		ui.Printf("Downloading %s", release.GetTagName())
		// get all assets
		assets, err := c.GetReleaseAssets(r.Owner, r.Name, release.GetID())
		if err != nil {
			return err
		}
		for _, asset := range assets {
			if asset.GetState() != "uploaded" {
				continue
			}
			ui.Printf("Downloading asset %s", asset.GetName())
			path := fmt.Sprintf("%s-%s/%s", repo.Name, release.GetTagName(), asset.GetName())
			// download asset
			rc, err := c.DownloadAsset(r.Owner, r.Name, asset.GetID())
			if err != nil {
				return err
			}
			// put rc to file
			data, err := io.ReadAll(rc)
			if err != nil {
				return err
			}
			for _, s := range storages {
				backend, err := storage.GetStorage(s)
				if err != nil {
					return err
				}
				err = backend.PutObject(path, data)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
