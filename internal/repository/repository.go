package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/google/uuid"
	"github.com/wnarutou/gitrieve/internal/archive"
	internalconfig "github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/lock"
	"github.com/wnarutou/gitrieve/internal/scm"
	"github.com/wnarutou/gitrieve/internal/scm/github"
	"github.com/wnarutou/gitrieve/internal/storage"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

// repoLister 抽象 GitHub 客户端，测试时替换 newGithubClient 即可 mock。
type repoLister interface {
	GetRepos(name, accountType string) ([]string, error)
}

type contextualRepoLister interface {
	GetReposContext(context.Context, string, string) ([]string, error)
}

// newGithubClient 是可替换的包级 seam：生产用真客户端，测试注入 fake。
var newGithubClient = func() (repoLister, error) { return github.New() }
var newGithubClientWithContext = func(ctx context.Context) (contextualRepoLister, error) { return github.NewWithContext(ctx) }

func shouldLogCloneError(isWiki bool, err error) bool {
	return !isWiki || !errors.Is(err, transport.ErrAuthenticationRequired)
}

func GetRepositories(name string) []typedef.Repository {
	repositories := make([]typedef.Repository, 0)
	if name != "" {
		// find repo in config
		for _, repository := range internalconfig.GetIns().Repository {
			if repository.Name == name || repository.Matches(name) {
				repositories = addRepo(repository, repositories)
			}
		}
		return repositories
	}
	for _, repository := range internalconfig.GetIns().Repository {
		repositories = addRepo(repository, repositories)
	}
	return repositories
}

func addRepo(repo typedef.Repository, ret []typedef.Repository) []typedef.Repository {
	return addRepoWithClient(repo, ret, newGithubClient)
}

func addRepoWithClient(repo typedef.Repository, ret []typedef.Repository, newClient func() (repoLister, error)) []typedef.Repository {
	switch repo.GetType() {
	case typedef.TypeRepo:
		ret = append(ret, repo)
	case typedef.TypeUser, typedef.TypeOrg:
		// get repos
		client, err := newClient()
		if err != nil {
			ui.Errorf("Error creating github client, %s", err)
			return ret
		}
		repos, err := client.GetRepos(repo.OrgName, repo.Type)
		if err != nil {
			ui.Errorf("Error getting user repos, %s", err)
			return ret
		}
		ret = appendConcreteRepositories(ret, repo, repos)
	default:
		ui.Errorf("Invalid repository type %s", repo.Type)
	}
	return ret
}

func appendConcreteRepositories(ret []typedef.Repository, configured typedef.Repository, repositories []string) []typedef.Repository {
	for _, repository := range repositories {
		ret = append(ret, typedef.Repository{
			Name:               path.Base(repository),
			URL:                repository,
			Cron:               configured.Cron,
			Storage:            configured.Storage,
			UseCache:           configured.UseCache,
			Type:               typedef.TypeRepo,
			AllBranches:        configured.AllBranches,
			Depth:              configured.Depth,
			DownloadReleases:   configured.DownloadReleases,
			DownloadIssues:     configured.DownloadIssues,
			DownloadWiki:       configured.DownloadWiki,
			DownloadDiscussion: configured.DownloadDiscussion,
		})
	}
	return ret
}

// Sync archives a repository's code (or wiki when iswiki is set). ctx bounds
// every go-git network operation: a caller cancellation (e.g. a user cancelling
// a job) or the internal 30-minute timeout fails the sync instead of hanging.
func Sync(ctx context.Context, repo typedef.Repository, iswiki bool, storages []typedef.MultiStorage) error {
	useCache := repo.UseCache
	depth := repo.Depth
	allBranches := repo.AllBranches
	isUpdated := false
	// get current directory
	currentDir, _ := os.Getwd()

	var workingDir string
	if useCache {
		workingDir = path.Join(currentDir, ".gitrieve")
	} else {
		id := uuid.New().String()
		workingDir = path.Join(currentDir, ".gitrieve", id)
	}

	// create a working directory if not exist
	err := storage.CreateDirIfNotExist(workingDir)
	if err != nil {
		ui.Errorf("Error creating working directory, %s", err)
		return err
	}

	// get the repo name from the URL
	r, err := scm.NewRepository(repo.URL)
	if err != nil {
		return err
	}
	repoName := r.Name
	// check if repo name is valid
	if repoName == "." || repoName == "/" {
		ui.Errorf("Invalid repository name")
		return err
	}

	// Serialize concurrent syncs of the same repo+component across goroutines
	// and processes (daemon overlap, executor re-trigger, parallel CLI runs).
	// go-git has no internal locking; two writers to the same .git corrupt the
	// object database. Keyed by (repo, component): code and wiki use different
	// keys so they stay parallel. Held across the whole sync so the
	// clone-failure RemoveAll can never delete a directory another sync is
	// actively using.
	component := "code"
	if iswiki {
		component = "wiki"
	}
	unlock, err := lock.Acquire(ctx, r, component, currentDir)
	if err != nil {
		return err
	}
	defer unlock()
	var gitDir string
	var gitUrl string
	if iswiki {
		gitDir = path.Join(workingDir, r.Host, r.Owner, repoName, "wiki")
		gitUrl = repo.URL + ".wiki"
	} else {
		gitDir = path.Join(workingDir, r.Host, r.Owner, repoName, "code")
		gitUrl = repo.URL
	}
	var exist bool
	// check if the repo already exists
	if _, err := os.Stat(path.Join(gitDir, git.GitDirName)); err == nil {
		exist = true
	}
	var gitRepo *git.Repository
	// Bound every go-git network operation (clone, fetch, remote list, pull).
	// Derive from the caller's ctx so a job cancellation also interrupts them,
	// and apply a 30-minute stall-detection timeout on top (generous; not a cap
	// on legitimate large clones).
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	// Route git's own progress (server-side "Enumerating/Counting/Compressing
	// objects" lines) into the log sink so a long clone/fetch streams live
	// status to the UI instead of leaving it silent until the sync completes.
	// One writer is shared across clone/fetch/pull so the throttle window is
	// continuous for the whole sync.
	progress := &progressWriter{}
	accounts := newGitAccounts(ctx, r.Host)

	// clone the repo if it does not exist, otherwise pull
	if !exist {
		isUpdated = true
		err = accounts.do(syncCtx, func(auth transport.AuthMethod) error {
			_, cloneErr := git.PlainCloneContext(syncCtx, gitDir, false, &git.CloneOptions{
				URL:      "https://" + gitUrl,
				Auth:     auth,
				Progress: progress,
				Depth:    depth,
			})
			if cloneErr != nil {
				// Fresh clones may initialize .git before authentication fails.
				// Clean only this new checkout before trying another credential.
				if cleanupErr := os.RemoveAll(gitDir); cleanupErr != nil {
					return fmt.Errorf("cleaning failed clone: %w", cleanupErr)
				}
			}
			return cloneErr
		})

		if err != nil {
			if shouldLogCloneError(iswiki, err) {
				ui.Errorf("Error cloning repository, %s", err)
			}
			return err
		}
	}

	// open local repo
	gitRepo, err = git.PlainOpen(gitDir)
	if err != nil {
		ui.Errorf("Error opening repository, %s", err)
		return err
	}
	remote, err := gitRepo.Remote("origin")
	if err != nil {
		ui.Errorf("Error get remote, %s", err)
		return err
	}
	accounts.restrictRemote(remote.Config().URLs)

	// fetch all remote branches
	err = accounts.do(syncCtx, func(auth transport.AuthMethod) error {
		return gitRepo.FetchContext(syncCtx, &git.FetchOptions{
			RemoteName: "origin",
			RefSpecs: []config.RefSpec{
				config.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
			},
			Auth:     auth,
			Force:    true,
			Progress: progress,
		})
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		ui.Errorf("Error fetching remote branches, %s", err)
		return err
	}

	// get remote references
	refs, err := gitRepo.References()
	if err != nil {
		ui.Errorf("Error get remote references, %s", err)
		return err
	}

	// get worktree
	w, err := gitRepo.Worktree()
	if err != nil {
		ui.Errorf("Error get worktree, %s", err)
		return err
	}

	// get remote default branch
	var remoteDefaultBranchName string
	var remoteDefaultBranchRef plumbing.ReferenceName

	var remoteRefs []*plumbing.Reference
	err = accounts.do(syncCtx, func(auth transport.AuthMethod) error {
		var listErr error
		remoteRefs, listErr = remote.ListContext(syncCtx, &git.ListOptions{Auth: auth})
		return listErr
	})
	if err != nil {
		// The default branch cannot be determined without this listing, and a
		// cancellation must stop the sync here rather than falling through.
		ui.Errorf("Error get remote references, %s", err)
		return err
	}
	for _, ref := range remoteRefs {
		if ref.Name() == "HEAD" {
			remoteDefaultBranchRef = ref.Target()
			remoteDefaultBranchName = remoteDefaultBranchRef.Short()
		}
	}

	// find all remote branches
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().IsRemote() {
			// get remote branch name
			remoteBranchName := ref.Name().Short()
			localBranchName := remoteBranchName[len("origin/"):]

			// if not pull all branches and this branch is not default then skip
			if !allBranches && (remoteDefaultBranchName != localBranchName) {
				return nil
			}

			// create local branch reference
			branchRef := plumbing.NewBranchReferenceName(localBranchName)

			// check local branch exist or not
			_, err := gitRepo.Reference(branchRef, false)

			// if local branch not exist
			if err == plumbing.ErrReferenceNotFound {
				isUpdated = true
				// create local branch and switch to the new local branch
				err = w.Checkout(&git.CheckoutOptions{
					Branch: branchRef,
					Create: true,
					Force:  true,
					// create local branch basing on ref.Hash(), that means head of remote branch,
					//   to avoid content of this new branch is a copy of last local branch,
					//   which will cause non-fast-forward update.
					Hash: ref.Hash(),
				})
				if err != nil {
					ui.Errorf("Error checkout local branch %s, %s", localBranchName, err)
					return err
				}
				ui.Printf("local branch %s has been checked out. \n", localBranchName)

				// set upstream branch of local branch to avoid the local branch
				//   without a tracking remote branch
				err = gitRepo.CreateBranch(&config.Branch{
					Name:   localBranchName,
					Remote: "origin",
					Merge:  branchRef,
				})
				if err != nil {
					ui.Errorf("Error setting %s's upstream branch , %s", localBranchName, err)
					return err
				}

				ui.Printf("local branch %s has been set to track remote branch %s. \n", localBranchName, remoteBranchName)
			} else if err != nil {
				ui.Errorf("Error checking local branch %s existing, %s", localBranchName, err)
				return err
			} else {
				ui.Printf("local branch %s exists, skip creating. \n", localBranchName)
			}

			// switch to local branch, only after that we can do pull
			err = w.Checkout(&git.CheckoutOptions{
				Branch: branchRef,
				// abandon the modify of local
				Force: true,
			})
			if err != nil {
				ui.Errorf("Error checkout local branch %s, %s", localBranchName, err)
				return err
			}

			// pull from upstream branch
			err = accounts.do(syncCtx, func(auth transport.AuthMethod) error {
				return w.PullContext(syncCtx, &git.PullOptions{
					RemoteName:    "origin",
					ReferenceName: branchRef,
					// pull all commits, not only the latest
					Auth:     auth,
					Depth:    depth,
					Progress: progress,
				})
			})
			if err == git.NoErrAlreadyUpToDate {
				ui.Printf("local branch %s already up to date. \n", localBranchName)
			} else if err != nil {
				ui.Errorf("Error pulling local branch %s, %s", localBranchName, err)
				if syncCtx.Err() != nil || gitAccountError(err) {
					// Cancelled — stop the whole sync instead of continuing to
					// remaining branches. Exhausted authentication candidates
					// must also fail the job, preserving the existing local cache.
					return err
				}
			} else {
				isUpdated = true
				ui.Printf("local branch %s has successed pull from remote branch %s, already up to date. \n", localBranchName, remoteBranchName)
			}
		}
		return nil
	}); err != nil {
		// refs.ForEach stops at the first error, including a cancellation.
		ui.Errorf("Error updating branches: %s", err)
		return err
	}

	// switch to default branch
	err = w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(remoteDefaultBranchName),
		// abandon the modify of local
		Force: true,
	})
	if err != nil {
		ui.Errorf("Error checkout default branch %s, %s", remoteDefaultBranchRef, err)
		return err
	}

	if isUpdated {
		if syncCtx.Err() != nil {
			// Cancelled after the network work — skip archiving/storing; the
			// fetched objects are already preserved in the local cache.
			return syncCtx.Err()
		}
		var targetDir string
		if iswiki {
			targetDir = r.Name + "_wiki"
		} else {
			targetDir = r.Name
		}

		// Archive the working tree directly from gitDir. Create takes an
		// absolute path and never changes the process cwd, so it is safe to
		// run from concurrent job goroutines.
		buf, err := archive.Create(syncCtx, gitDir, targetDir)
		if err != nil {
			ui.Errorf("Error creating archive, %s", err)
			return err
		}

		// For codes, there is no need to save the history: the latest one is
		// the full version and already contains all the history, so it can be
		// replaced directly.
		base := targetDir + ".tar.gz"

		// handle storages
		for _, s := range storages {
			backend, err := storage.GetStorage(s)
			if err != nil {
				ui.Errorf("Error getting backend, %s", err)
				return err
			}
			err = backend.PutObject(path.Join(s.Path, r.Host, r.Owner, r.Name, base), buf.Bytes())
			if err != nil {
				ui.Errorf("Error storing file, %s", err)
				return err
			}
			ui.Printf("File %s stored", path.Join(s.Path, r.Host, r.Owner, r.Name, base))
		}
	} else {
		ui.Printf("All is uptodate, no need to restore")
	}

	if syncCtx.Err() != nil {
		// Cancelled — stop before any remaining work (e.g. temp-dir cleanup).
		return syncCtx.Err()
	}

	// cleanup
	if !useCache {
		err = os.RemoveAll(gitDir)
		if err != nil {
			ui.Errorf("Error cleaning up working directory, %s", err)
			return err
		}
	}
	return nil
}

// Expand 返回一个配置条目实际对应的具体仓库列表。type=repo 原样返回自身；
// type=user/org 通过 GitHub API 展开为成员仓库（继承 cron/storage 等选项）；
// 非法类型返回空切片。CLI 与 executor 共用。
func Expand(repo typedef.Repository) []typedef.Repository {
	return addRepo(repo, nil)
}

// ExpandContext expands a user/org entry using the caller's frozen execution
// configuration (token, retry, and API coordination scope when present).
func ExpandContext(ctx context.Context, repo typedef.Repository) ([]typedef.Repository, error) {
	switch repo.GetType() {
	case typedef.TypeRepo:
		return []typedef.Repository{repo}, nil
	case typedef.TypeUser, typedef.TypeOrg:
		client, err := newGithubClientWithContext(ctx)
		if err != nil {
			return nil, err
		}
		repositories, err := client.GetReposContext(ctx, repo.OrgName, repo.Type)
		if err != nil {
			return nil, err
		}
		return appendConcreteRepositories(nil, repo, repositories), nil
	default:
		return nil, fmt.Errorf("invalid repository type %q", repo.Type)
	}
}
