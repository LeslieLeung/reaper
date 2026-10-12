package issue

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	gh "github.com/google/go-github/v56/github"
	"github.com/google/uuid"
	"github.com/wnarutou/gitrieve/internal/archive"
	"github.com/wnarutou/gitrieve/internal/config"
	"github.com/wnarutou/gitrieve/internal/githubapi"
	"github.com/wnarutou/gitrieve/internal/lock"
	"github.com/wnarutou/gitrieve/internal/retry"
	"github.com/wnarutou/gitrieve/internal/scm"
	"github.com/wnarutou/gitrieve/internal/storage"
	"github.com/wnarutou/gitrieve/internal/typedef"
	"github.com/wnarutou/gitrieve/internal/ui"
)

func newIssueListOptions(lastUpdate time.Time) *gh.IssueListByRepoOptions {
	return &gh.IssueListByRepoOptions{
		State:     "all",
		Since:     lastUpdate.UTC(),
		Sort:      "updated",
		Direction: "asc",
		ListOptions: gh.ListOptions{
			PerPage: 100,
		},
	}
}

// nextIssuePageURL preserves the complete next link, including opaque cursors.
// NextPage alone loses after/before parameters and cannot identify cursor-only pages.
func nextIssuePageURL(resp *gh.Response) string {
	for _, header := range resp.Header.Values("Link") {
		for _, link := range strings.Split(header, ",") {
			segments := strings.Split(link, ";")
			target := strings.TrimSpace(segments[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, parameter := range segments[1:] {
				name, value, ok := strings.Cut(parameter, "=")
				if !ok || strings.TrimSpace(name) != "rel" {
					continue
				}
				for _, relation := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
					if relation == "next" {
						return target[1 : len(target)-1]
					}
				}
			}
		}
	}
	return ""
}

func Sync(ctx context.Context, repo typedef.Repository, storages []typedef.MultiStorage) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	isUpdated := false
	useCache := repo.UseCache
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

	// Serialize concurrent syncs of the same repo's issues (in-process and
	// cross-process): they share the .gitrieve/issues cache dir and the
	// issues.tar.gz storage path.
	unlock, err := lock.Acquire(ctx, r, "issue", currentDir)
	if err != nil {
		return err
	}
	defer unlock()
	gitDir := path.Join(workingDir, r.Host, r.Owner, repoName, "issues")
	err = storage.CreateDirIfNotExist(gitDir)
	if err != nil {
		ui.Errorf("Error creating working directory, %s", err)
		return err
	}
	if !useCache {
		defer func() {
			if err := os.RemoveAll(gitDir); err != nil {
				ui.Errorf("Error cleaning up working directory, %s", err)
				return
			}
			ui.Printf("Cleanup completed for directory: %s", gitDir)
		}()
	}
	// Get all issue files in the gitDir directory
	files, err := os.ReadDir(gitDir)
	if err != nil {
		ui.Errorf("Error reading issue directory: %s", err)
		return err
	}

	var lastUpdate time.Time
	if len(files) == 0 {
		// Keep the zero value so go-github omits the since filter and the
		// first sync downloads every issue and pull request.
		ui.Printf("No issues downloaded yet, need to download all issues")
	} else {
		// Traverse all issue files to get the latest update time
		var updateTime time.Time
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".md") {
				continue
			}

			content, err := os.ReadFile(path.Join(gitDir, file.Name()))
			if err != nil {
				ui.Errorf("Error reading issue file: %s", err)
				return err
			}

			// Parse the markdown file content to get the update time
			lines := strings.Split(string(content), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "- Updated Time: ") {
					timeStr := strings.TrimPrefix(line, "- Updated Time: ")
					timeStr = strings.TrimSpace(timeStr)
					// use UTC time to avoid be affect by local time zone
					loc, _ := time.LoadLocation("UTC")
					updateTime, err = time.ParseInLocation("2006-01-02 15:04:05", timeStr, loc)
					if err != nil {
						continue
					}
					break
				}
			}

			if updateTime.After(lastUpdate) {
				lastUpdate = updateTime
			}
		}
		ui.Printf("The latest update time among all issues is: %s", lastUpdate)
	}

	// A zero lastUpdate omits since for the initial full sync. Incremental
	// syncs use the same instant in UTC without reinterpreting wall-clock time.
	opt := newIssueListOptions(lastUpdate)

	cfg := config.GetExecutionConfig(ctx)
	client := gh.NewClient(nil).WithAuthToken(cfg.GitHubToken)
	var nextURL string
	for page := 1; ; page++ {
		var (
			issues []*gh.Issue
			resp   *gh.Response
		)
		err := retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
			permit, acquireErr := githubapi.Acquire(ctx, "core")
			if acquireErr != nil {
				return acquireErr
			}
			var apiErr error
			if nextURL == "" {
				issues, resp, apiErr = client.Issues.ListByRepo(ctx, r.Owner, r.Name, opt)
			} else {
				var req *http.Request
				req, apiErr = client.NewRequest(http.MethodGet, nextURL, nil)
				if apiErr == nil {
					resp, apiErr = client.Do(ctx, req, &issues)
				}
			}
			permit.Done(githubapi.ObserveREST(resp, apiErr))
			return apiErr
		})
		if err != nil {
			ui.Errorf("Error fetching issues, %s", err)
			return err
		}
		ui.Printf("Fetching page %d, total %d issues", page, len(issues))

		// Verified that for each issue, if the issue or any comment under it is updated, the issue's update time will be updated
		// Traverse all issues
		for _, issue := range issues {
			isUpdated = true
			// Get all comments under the issue
			commentsOpt := &gh.IssueListCommentsOptions{
				ListOptions: gh.ListOptions{
					PerPage: 100,
				},
			}
			var allComments []*gh.IssueComment
			for {
				var (
					comments []*gh.IssueComment
					resp     *gh.Response
				)
				err := retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
					permit, acquireErr := githubapi.Acquire(ctx, "core")
					if acquireErr != nil {
						return acquireErr
					}
					var apiErr error
					comments, resp, apiErr = client.Issues.ListComments(ctx, r.Owner, r.Name, issue.GetNumber(), commentsOpt)
					permit.Done(githubapi.ObserveREST(resp, apiErr))
					return apiErr
				})
				if err != nil {
					ui.Errorf("Error fetching comments of issue %d, %s", issue.GetNumber(), err)
					return err
				}
				allComments = append(allComments, comments...)

				if resp.NextPage == 0 {
					break
				}
				commentsOpt.Page = resp.NextPage
			}

			// Create issue file
			issueFileName := fmt.Sprintf("#%d.md", issue.GetNumber())
			issueFilePath := path.Join(gitDir, issueFileName)

			// Generate markdown content
			var content string
			if issue.IsPullRequest() {
				content += fmt.Sprintf("# PullRequest #%d: %s\n\n", issue.GetNumber(), issue.GetTitle())
			} else {
				content += fmt.Sprintf("# Issue #%d: %s\n\n", issue.GetNumber(), issue.GetTitle())
			}
			content += "## Basic Information\n\n"
			content += fmt.Sprintf("- Created Time: %s\n", issue.GetCreatedAt().Format("2006-01-02 15:04:05"))
			content += fmt.Sprintf("- Updated Time: %s\n", issue.GetUpdatedAt().Format("2006-01-02 15:04:05"))
			content += fmt.Sprintf("- State: %s\n", issue.GetState())
			content += fmt.Sprintf("- Author: %s\n", issue.GetUser().GetLogin())
			content += fmt.Sprintf("- Comment Count: %d\n\n", len(allComments))

			content += "## Content\n\n"
			content += "```\n\n"
			content += issue.GetBody() + "\n\n"
			content += "```\n\n"

			if len(allComments) > 0 {
				content += "## Comments\n\n"
				for _, comment := range allComments {
					content += fmt.Sprintf("### Comment #%d\n\n", comment.GetID())
					content += "```\n\n"
					content += comment.GetBody() + "\n\n"
					content += "```\n\n"
					content += fmt.Sprintf("- Author: %s\n", comment.GetUser().GetLogin())
					content += fmt.Sprintf("- Created Time: %s\n", comment.GetCreatedAt().Format("2006-01-02 15:04:05"))
					content += fmt.Sprintf("- Updated Time: %s\n\n", comment.GetUpdatedAt().Format("2006-01-02 15:04:05"))
					content += "---\n\n"
				}
			}

			// Write to file
			err = os.WriteFile(issueFilePath, []byte(content), 0644)
			if err != nil {
				ui.Errorf("Error writing issue file %s, %s", issueFilePath, err)
				return err
			}
			ui.Printf("Success writing issue #%d to file %s", issue.GetNumber(), issueFilePath)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		nextURL = nextIssuePageURL(resp)
		if nextURL == "" {
			break
		}
	}

	if isUpdated {
		// Archive the issues dir directly from gitDir. Create takes an
		// absolute path and never changes the process cwd, so it is safe to
		// run from concurrent job goroutines.
		buf, err := archive.Create(ctx, gitDir, "issues")
		if err != nil {
			ui.Errorf("Error creating archive, %s", err)
			return err
		}

		base := "issues.tar.gz"

		// Handle storages
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
		ui.Printf("All is up to date, no need to restore")
	}

	return nil
}
