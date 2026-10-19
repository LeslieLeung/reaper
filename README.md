# gitrieve

English | [简体中文](README_zh.md)

Git Retrieve(gitrieve) is a tool to archive repositories from any Git servers.

- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)
  - [repository](#repository)
  - [run](#run)
  - [release](#release)
  - [daemon](#daemon)
- [Web UI](#web-ui)
- [Configuration](#configuration)
- [Storage](#storage)
- [Deletion-safe sync](#deletion-safe-sync)
- [Run as docker container](#run-as-docker-container)
  - [Docker CLI](#docker-cli)
  - [Docker Compose](#docker-compose)

## Features

- Archive repositories from any Git servers
- Archive repositories of a user/an organization (see [Configuration](https://github.com/wnarutou/gitrieve/wiki/Configuration#repository))
- Cron support
- Local file storage (see [Storage](#storage))
- **Deletion-safe sync** — local cached code and full history are never deleted by a sync, even when the upstream repo is taken down, DMCA-disabled, deleted, or replaced with a single README (see [Deletion-safe sync](#deletion-safe-sync))
- Docker support (see [Run as docker container](#run-as-docker-container))

## Installation

```bash
curl -sSfL https://raw.githubusercontent.com/wnarutou/gitrieve/main/install.sh | sh -s -- -b /usr/local/bin
```

Or get from [Release](https://github.com/wnarutou/gitrieve/releases).

## Usage

You have to create a configuration file to use gitrieve.

```yaml
repository:
  - name: gitrieve
    url: github.com/wnarutou/gitrieve
    cron: "0 * * * *"
    storage:
      - localFile
    useCache: True
    allBranches: True
    depth: 0
    downloadReleases: True
    downloadIssues: True
    downloadWiki: True
    downloadDiscussion: True

storage:
  - name: localFile
    type: file
    path: ./repo
```

Then you can run gitrieve with the configuration file.

```bash
gitrieve -c config.yaml
# or simply call gitrieve if your configuration file is named config.yaml
gitrieve
```

### repository

`repository` archives a single repository defined in configuration.

Git clone, fetch, pull, and remote-reference queries have no fixed total time
limit. A network read fails only after five minutes without receiving any
bytes; incoming data renews that window even without progress messages.
Local decoding, checkout, and archiving are not subject to this network timeout.
Cancelling a job still interrupts its network operations.

```bash
gitrieve repository gitrieve
```

### run

`run` archives all repositories defined in configuration.

```bash
gitrieve run
```

Combined with cron, you can archive repositories periodically.

### release

`release` archives all release assets of a repository.

```bash
gitrieve release gitrieve
```

### daemon

`daemon` runs gitrieve as a daemon. It will archive all repositories defined in configuration periodically.

```bash
gitrieve daemon
# You might want to run it with something like nohup
nohup gitrieve daemon &
```

## Web UI

`server` starts a web UI and HTTP API for managing archive jobs and configuration through a browser. It also runs the repository cron schedules, so a separate `daemon` process is not required.

```bash
gitrieve server
# By default it listens on http://localhost:8080
```

From the UI you can trigger archive jobs, view real-time logs, and edit repository/storage configuration without touching `config.yaml` by hand. Repository cron schedules are refreshed immediately after repository edits, config imports, and config reloads. The server is configured via an optional `server` section in `config.yaml` (host, port, and optional bearer-token auth).

Execution logs are cleaned in the background at startup and every 24 hours.
Set `server.logRetentionMonths` to the number of calendar months to retain
(default: `2`; non-positive values use the default). Cleanup removes all logs
belonging to terminal jobs whose **end time** precedes the cutoff. Active jobs,
jobs without an end time, and jobs ending exactly at the cutoff retain their
logs. Job history and component outcomes remain available. Restart or reload
configuration after changing the setting; the next cleanup pass uses the new value.

The Repositories page provides server-side health counts, failed/never/overdue/
stuck filters, attention-first sorting, component detail, and safe bulk retry.
It distinguishes the latest attempt from the latest fully successful backup.
Stuck includes executions that remain either pending or running beyond
`syncStuckThreshold`.
An unsupported enabled component is recorded as skipped; a real component
failure makes the overall execution fail. The page **never refreshes
automatically** from polling, timers, or job-completion SSE events: use its
Refresh button (or revisit the page) when you want a new snapshot.

See the [Web UI guide](docs/web-ui.md) and the [API reference](docs/api.md) for details.

## Configuration

GitHub API traffic is coordinated within each gitrieve process. This example uses tokens belonging to two different GitHub users:

```yaml
githubAccounts:
  - name: alice
    token: alice-personal-access-token
  - name: bob
    token: bob-personal-access-token
githubApiConcurrency: 4
githubAccountConcurrency: 2
githubMinRequestInterval: 200ms
githubLowRemainingThreshold: 100
githubScheduleJitter: 30s
syncOverdueGrace: 30m
syncStuckThreshold: 24h
cocurrencyNum: 12
```

`githubAccounts` defaults to an empty list. A nonempty pool takes precedence over `githubToken`; otherwise the legacy token is used, or REST runs anonymously when no token is set. Account names and tokens must be nonblank, and names must be unique. Each pool account has a concurrency cap (`githubAccountConcurrency`, default `2`); all accounts also share the process-wide cap (`githubApiConcurrency`, default `2`). Legacy single-token authentication retains its global-only concurrency cap. `cocurrencyNum` limits concurrent repository jobs, independently of these API limits.

REST and GraphQL share pacing and concurrency for each credential. `githubMinRequestInterval` spaces request starts per credential, while primary quota pauses apply to the affected account and API resource. A secondary rate limit pauses the whole process. Duplicate tokens share the same limits, and multiple tokens for one GitHub user share that user's quota. A pool of different users can use their aggregate available quotas; it does not increase any user's GitHub quota. For private repositories, pool users need access and their tokens need the required permissions; an account without access may fall back to another eligible account.

HTTPS Git operations for GitHub code repositories and wikis also use the account pool. Each clone, fetch, remote-reference listing, and pull keeps one credential for its entire attempt. Authentication, authorization, or repository-not-found errors corresponding to 401, 403, or 404 rotate through distinct tokens in configured order, with at most one attempt per token for that operation. Later operations in the same job prefer the last successful account. If every account is denied, the job fails and account rotation does not delete an existing cache; failed fresh clone directories are cleaned before another attempt. Without credentials, Git remains anonymous; GitHub tokens are never sent to other repository hosts. Git transfers use repository-job concurrency and do not occupy API concurrency slots. Private-repository tokens also need permission to read code.

Import previews keep account labels and mask every token. The `existing` and `imported` global choices apply separately to `githubAccounts` and `githubAccountConcurrency`; exports and saved configuration contain the actual tokens.

`githubScheduleJitter: 0s` disables staggering. Staggering applies only to daemon cron jobs; manual commands and Web API jobs start immediately. Pacing and concurrency settings reduce bursts; coordination does not extend to separate processes or hosts.

`syncOverdueGrace` and `syncStuckThreshold` default to `30m` and `24h`;
the stuck threshold applies to both pending and running executions, and
non-positive values select those defaults. The existing, intentionally spelled
`cocurrencyNum` setting limits actual running executor work across server cron,
manual, and bulk requests. Extra work remains pending, and duplicate active
work for one repository is rejected. On restart, the server marks executions
and component rows left pending/running by the prior process as failed so they
cannot remain active forever.

For configuration, you can check out this [example](config/example.config.yaml).

For more details, see [Configuration](https://github.com/wnarutou/gitrieve/wiki/Configuration) in wiki.

## Storage

gitrieve currently supports local file storage only. S3-compatible storage is no longer supported; existing configurations with `type: s3` must be changed to `type: file` with a local `path` before starting, reloading, or importing configuration.

- [x] File

## Deletion-safe sync

A core design goal of gitrieve is **once code and history have been pulled locally, a sync must never delete them** — even if the upstream repository is taken down, DMCA-disabled, deleted, made private, or replaced with a single README. This makes gitrieve suitable as a true archive/backup tool rather than a mere mirror.

How this is guaranteed by the sync logic:

- **Remote unreachable → early exit, nothing touched.** If the upstream repo is deleted, disabled (e.g. DMCA), or made private without access, the `fetch` fails and the sync returns early. No archiving runs, no cleanup runs, and the local cache plus any previously archived snapshots are left untouched.
- **Branches are only added, never deleted.** The sync iterates remote branches and creates/updates local branches accordingly; it never removes a local branch. Branches that the upstream deleted still live on locally.
- **Pull, not reset.** Updates are applied via `git pull` (merge), never `git reset --hard origin`. A force-push that rewrites the upstream default branch only moves the `origin/*` tracking refs; your local branches and their old commit objects are not overwritten or discarded.
- **Old commits are retained.** Because commits are immutable objects and the sync never force-moves local refs, the full history you have already pulled stays in the local `.git` object store. Any past commit can be recovered with `git checkout <old-hash>`.

Recommended configuration for maximum recoverability:

- `allBranches: true` — ensures every branch's commits are pulled into the local object store.
- `useCache: true` — keeps the local cache directory (with its `.git`) across syncs as an extra on-disk safety net (without it the working dir is removed at the end of each sync).

One caveat to be aware of: archiving writes to a fixed filename (e.g. `repo.tar.gz`) and overwrites any previous archive at the same path. So if an upstream is still reachable but its default branch has been rewritten to a single README, the *new* snapshot will replace the previous normal one at that path. Local cached code and history are still safe (see above), but to preserve distinct historical snapshots you should keep separate copies of archives under versioned paths.

## Run as docker container

### Docker CLI

One-off run. 
- Change `${pwd}/config/example.config.yaml` to your config file path.
- Customize `${pwd}/repo:/repo` to be your desired storage path. The in-container path needs to be the same as the path in config file.

```bash
docker run --rm \
    -v ${pwd}/config/example.config.yaml:/config.yaml \
    -v ${pwd}/repo:/repo \
    wnarutou/gitrieve:latest \
    run
```

### Docker Compose

For example compose file, see [docker-compose.yml](docker-compose.yml).

```bash
docker compose up -d
```

The released image is a multi-arch manifest covering `linux/amd64` and
`linux/arm64`, so it works on both x86_64 and Apple Silicon machines.

### Maintenance before upgrades

Click **进入维护模式** (Enter maintenance) at the top of the Web UI and wait for
**可以安全升级** (Ready to upgrade), then update the container:

```bash
docker compose pull
docker compose up -d
```

Maintenance rejects new submissions and cancels queued executions. Running
Git/Wiki network operations stop; Issue/Discussion finish the current list page,
including its comments/replies, and publish the downloaded data as an archive.
Release finishes downloading and storing its current file. Archive writes already
in progress finish before shutdown. Download/storage errors remain failures.

With `useCache: false`, an incomplete Issue/Discussion list is published as a
separate `issues.partial.<ID>.tar.gz` / `discussions.partial.<ID>.tar.gz`, preserving
the previous complete archive. Partial artifacts are retained; a later full sync
updates the canonical archive normally. Cached syncs republish their archives on
subsequent successful runs even without upstream changes, repairing interrupted
or failed writes at the cost of additional compression and upload work.

Maintenance is process-local. After restart, normal cron scheduling resumes;
old queued/interrupted jobs are not replayed immediately. The next scheduled or
manual invocation creates a new execution. Repositories without cron require a
manual invocation. Without restarting, use **退出维护模式** (Leave maintenance)
after draining to reopen admission without submitting any jobs.

In `server` mode, SIGTERM/SIGINT uses the same drain. The example Compose file
allows `stop_grace_period: 10m`; Docker may still force termination after that
deadline. Wait for readiness in the UI first when files/pages can take longer.
Database persistence failures prevent a ready indication; inspect logs and resolve
the storage problem before proceeding.

The authenticated maintenance API is `GET /api/maintenance` (status),
`POST /api/maintenance` (enter), and `DELETE /api/maintenance` (leave).
`ready: true` means execution work and state persistence have settled. Maintenance
status refreshes never refresh the Repositories list automatically.
