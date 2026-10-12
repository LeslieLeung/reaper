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

GitHub API traffic from issues, discussions, and releases is coordinated within each gitrieve process. Optional settings and defaults:

```yaml
githubApiConcurrency: 2
githubMinRequestInterval: 200ms
githubLowRemainingThreshold: 100
githubScheduleJitter: 30s
syncOverdueGrace: 30m
syncStuckThreshold: 24h
cocurrencyNum: 6
```

`githubScheduleJitter: 0s` disables staggering. Staggering applies only to daemon cron jobs; manual commands and Web API jobs start immediately. These settings reduce bursts but do not increase GitHub quota and do not coordinate separate processes or hosts.

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
