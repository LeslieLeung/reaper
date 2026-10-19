# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**gitrieve** is a Go-based tool for archiving repositories from any Git server (GitHub, etc.) to local file storage. It supports scheduling via cron, downloading repository metadata (releases, issues, wiki, discussions), and storing to the local filesystem.

## Build & Development

```bash
# Build
go build -o gitrieve main.go

# Run
./gitrieve -c config.yaml
./gitrieve              # uses config.yaml by default

# Commands
./gitrieve run          # archive all repos in config
./gitrieve repository <name>  # archive single repo
./gitrieve release <name>     # download release assets
./gitrieve daemon       # run cron schedules without the Web UI/API
./gitrieve server       # run Web UI/API and cron schedules together

# Release (tagged pushes only)
# Uses GoReleaser; builds multi-arch binaries and Docker image
```

## Architecture

### Directory Structure
- `cmd/` - Cobra CLI commands (root, run, repository, daemon, release, issue, wiki, discussion)
- `internal/` - Core logic organized by domain:
  - `config/` - Viper-based configuration loading
  - `repository/` - Git cloning/fetching logic
  - `scm/` - SCM abstraction (currently GitHub via `githubv4`/`go-github`)
  - `storage/` - Local file storage
  - `typedef/` - Shared types (Repository, Storage, MultiStorage)
  - `ui/` - Terminal output helpers

### Key Patterns
- Configuration initialized via `cobra.OnInitialize(config.Init)` in root.go
- Config parsed into `Config` struct with `Repository[]`, `Storage[]`, and global settings
- Storage backends selected by name reference in repository config
- Daemon and server modes use `gocron/v2` for scheduled jobs
- Archives created with `archiver/v4`, stored by direct file write

### Concurrency: cross-process per-repo locks
Same repo + same component syncs are serialized across goroutines AND processes
by `internal/lock` — a per-key in-process semaphore plus a `gofrs/flock` advisory
file lock on `.gitrieve/locks/<host>/<owner>/<repo>/<component>.lock`. Covers
code/wiki/issue/discussion/release. The lock is per-host and per-working-directory:
multi-host writes to shared storage (e.g. two machines writing one network filesystem) are
not guarded. Lock files are never deleted.

### Server repository sync health

`executions` remains the historical source of truth; do not add a materialized
repository-state table without a separate design. `internal/db.RepositoryRunStats`
uses `idx_executions_repo_start` for each latest attempt and
`idx_executions_repo_status_end` for counts and latest success. Preserve those
access paths and the 6,000 repositories x 100 executions benchmark when changing
the overview query.

Server cron, `POST /api/jobs`, and `POST /api/jobs/bulk` all enqueue through the
same `Executor`. `cocurrencyNum` (intentional legacy spelling) caps actual
running work, not just scheduler callbacks. Pending and running work must remain
cancellable, and a repository with an active execution must not be enqueued a
second time. Startup reconciles orphaned pending/running executions and active
component rows to failed.

Overall `completed` means every enabled component is `completed` or `skipped`.
Any enabled component failure makes the execution `failed`, but ordinary
failure does not prevent later enabled components from running. A skip is only
for a recognizable unsupported capability, never for permission, network,
storage, parsing, or rate-limit errors. Historical executions may legitimately
have no component rows.

Repository health precedence is never-synced, stuck, active pending/running,
failed/cancelled, overdue, then healthy. Keep last attempt separate from last
success, and compute summary counts across all search matches before health/
diagnostic filters and pagination. Bulk retry must recompute and confirm the
current eligible count, then account for partial outcomes.

Stuck classification applies when the latest execution has remained either
`pending` or `running` beyond `syncStuckThreshold`; do not narrow it to only
running work.

**Critical UI invariant:** the Repositories page refreshes only on an explicit
user request. Never add polling, timer-triggered reloads, or SSE-completion
reloads for this page.

### Deletion-safe sync (critical invariant — do not regress)
A core design goal: **once code and history are pulled locally, a sync must never delete them**, even when the upstream repo is taken down, DMCA-disabled, deleted, made private, or replaced with a single README. This makes gitrieve a true archive/backup tool, not a mirror. When modifying `internal/repository/repository.go` (`Sync`), preserve these guarantees:

- **Remote unreachable → early exit.** `gitRepo.Fetch` failure returns early (`repository.go` ~line 159-162); no archiving and no `os.RemoveAll` cleanup runs, so the local cache and prior archived snapshots are untouched. Do NOT move cleanup before fetch or swallow fetch errors to continue.
- **Branches are added, never deleted.** The `refs.ForEach` loop (`repository.go` ~line 199-286) only creates/updates local branches; it never deletes a local branch. Upstream-deleted branches must remain locally.
- **Fast-forward only, never reset.** The default branch uses `w.Pull`; non-default branches update local refs only after proving ancestry, respecting shallow-history boundaries. Never use `git reset --hard origin` or overwrite local branch history after an upstream force-push.
- **Only materialize the default branch.** Non-default branches are archived as Git refs and objects without checkout, so filenames that exceed host filesystem limits do not break the backup. A cache left on another branch by a failed checkout must return to the default branch and produce an archive on retry.
- **Old commits retained.** Commits are immutable objects and the sync never force-moves local refs, so already-pulled history stays in the local `.git` object store and is recoverable via `git checkout <old-hash>`.

Recommended-for-recoverability config: `allBranches: true` (pull every branch's commits) and `useCache: true` (keep the local `.git` cache across syncs; otherwise the working dir is removed at the end of each sync).

Known limitation (not yet a regression to fix unless asked): archiving writes a fixed filename (e.g. `repo.tar.gz`) and overwrites any prior archive at the same path. If the upstream is still reachable but its default branch was rewritten to a single README, the new snapshot replaces the previous normal one at that path. Local cached code/history is still safe, but distinct historical snapshots need separate copies under versioned archive paths.

### Configuration Schema
```yaml
repository:
  - name: <id>
    url: <host/owner/repo>        # OR use orgName + type: user/org
    cron: "<cron expression>"
    storage: [<storage names>]
    useCache: true/false
    allBranches: true/false
    depth: <int, 0=all>
    downloadReleases/Issues/Wiki/Discussion: true/false

storage:
  - name: <id>
    type: file
    path: <local path>

githubToken: <token>
cocurrencyNum: <int>
releaseSizeLimit: <bytes>
releaseNumLimit: <count>
retryMaxCount: <int, default 3>    # per-call max retries on rate-limit/5xx/network errors
retryBaseDelay: <duration, default 5s>  # exponential-backoff base (doubles per retry)
githubApiConcurrency: <uint, default 2>
githubMinRequestInterval: <duration, default 200ms>
githubLowRemainingThreshold: <int, default 100>
githubScheduleJitter: <duration, default 30s; 0s disables daemon staggering>
syncOverdueGrace: <duration, default 30m>
syncStuckThreshold: <duration, default 24h>
```

## Testing

Go tests live alongside the code they test as `_test.go` files (e.g. `internal/retry/retry_test.go`, `internal/lock/lock_test.go`), following Go conventions. Run the full Go and web suite with `make test`; the underlying commands are `go test ./...` and `node --test web/main.test.cjs web/tests/*.test.js`.
