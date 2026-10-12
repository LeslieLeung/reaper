# Web UI Guide

gitrieve ships with a built-in web UI for managing archive jobs and configuration through a browser, instead of the CLI commands. This guide covers how to start the server, access the UI, and configure it.

## Starting the server

Build gitrieve and launch the server command:

```bash
gitrieve build -o gitrieve main.go
# or, if already installed:
gitrieve server
```

You must have a `config.yaml` in place (or pass `-c <path>`), just like the CLI commands. The server reads your repository and storage configuration and lets you manage them at runtime.

```bash
gitrieve -c config.yaml server
```

On startup you will see:

```
Starting server on localhost:8080
```

## Accessing the UI

Open your browser at:

```
http://localhost:8080
```

The default host is `localhost` and the default port is `8080`. Static assets (CSS/JS) are served from `/static/*`.

## Features

### Job management

- **Trigger an archive** — start a job for any repository defined in your config.
- **View job history** — list past and current jobs with status, start/end time, and error messages. Filter by status or repository, and paginate.
- **Cancel a job** — stop a running or pending job.
- **Real-time logs** — every job has a live log stream that updates as the job runs, using Server-Sent Events. The stream closes automatically when the job finishes.

### Log retention

The server cleans historical execution logs in the background at startup and
every 24 hours. `server.logRetentionMonths` controls the retention period in
calendar months (default: `2`). Missing, zero, or negative values use the default.
For example:

```yaml
server:
  logRetentionMonths: 2
```

Cleanup uses the **execution's end time**, not each log line's timestamp. All log
lines for a completed, failed, or cancelled execution are removed only when its
`end_time` is strictly earlier than the retention cutoff. Pending/running
executions, executions without an end time, and executions ending exactly at the
cutoff keep their logs. If the target month is shorter, its last day is used
(for example, March 31 minus one month is February 28, or 29 in a leap year).

Execution records and component outcomes remain available for job history and
repository health statistics. Deletion is batched, and log lookups use an index.
SQLite reuses the freed database pages; cleanup does not run a blocking `VACUUM`.
Restart the server or reload configuration to pick up a changed retention value;
the next cleanup pass reads the current setting.

### Repository sync health

The Repositories page is a server-paginated fleet overview. Summary cards show
total, healthy, failed, syncing, never-synced, and overdue counts across every
repository matching the current text search, not only the visible page. Health,
overdue, and stuck filters plus attention/name/last-attempt/last-success sorting
are kept in the URL hash, so exception views can be bookmarked.

Each row deliberately shows both the last attempt and last successful
completion. A recent failed attempt must not make an older successful backup
look recent. Primary health uses this precedence: never synced; stuck; pending
or running; failed or cancelled; overdue; healthy. Overdue and stuck are also
shown as independent diagnostics. A repository is stuck when its latest
execution remains `pending` or `running` beyond `syncStuckThreshold`.

Open a row's execution detail to see the exact latest execution, its component
outcomes, and its logs. Code is always enabled; release, issues, wiki, and
discussion appear when configured. Unsupported capabilities are `skipped` and
do not fail an execution. A real failure in any enabled component makes the
overall execution `failed`, although later components are still attempted.
Older executions created before component tracking show that component details
are unavailable instead of inventing results.

Bulk retry operates on the complete server-side failed/cancelled/overdue
selection. The confirmation displays the eligible count. If that count changes
before submission, the server returns the new count and the UI asks again.
Active or no-longer-eligible repositories are safely accounted for, and one
enqueue failure does not stop the remaining repositories.

**Refresh is always explicit.** The Repositories page never polls, schedules a
timer reload, or reloads because a job's SSE stream completes. Click **Refresh**
or revisit the page to see later server-side changes.

### Configuration management

- **Repositories** — add, edit, and remove repository entries from your `config.yaml` without editing the file by hand. Changes are persisted back to the config file.
- **Storage backends** — add, edit, and remove `file` and `s3` storage backends. Changes are persisted back to the config file.

All write operations update the in-memory config immediately and then attempt to save it to disk. If the save fails (for example, the config file is read-only), the change still takes effect for the running server, and the API response will carry a warning message.

## Configuration

The server is configured via an optional `server` section in `config.yaml`:

```yaml
server:
  host: localhost        # bind address
  port: 8080             # listen port
  authEnabled: false     # require a bearer token for API access
  authToken: ""          # the token clients must send when authEnabled is true
```

| Field | Default | Description |
|---|---|---|
| `host` | `localhost` | Network address to bind |
| `port` | `8080` | Port to listen on |
| `authEnabled` | `false` | When `true`, API requests must include the bearer token |
| `authToken` | `""` | Token value sent as `Authorization: Bearer <token>` |

A complete example combining the server section with the rest of the config:

```yaml
server:
  host: 0.0.0.0
  port: 8080
  authEnabled: true
  authToken: "change-me-in-production"

repository:
  - name: gitrieve
    url: github.com/wnarutou/gitrieve
    cron: "0 * * * *"
    storage:
      - localFile
    useCache: true
    allBranches: true
    depth: 0
    downloadReleases: true
    downloadIssues: true
    downloadWiki: true
    downloadDiscussion: true

storage:
  - name: localFile
    type: file
    path: ./repo

githubToken: your-github-token
syncOverdueGrace: 30m
syncStuckThreshold: 24h
cocurrencyNum: 6
```

The `server` process executes every non-empty repository `cron` schedule. Cron
registrations are refreshed immediately when repositories are created, updated,
or deleted through the API, and after config import or reload.

`syncOverdueGrace` defaults to `30m` and prevents a repository from being
declared overdue immediately after its cron time. `syncStuckThreshold` defaults
to `24h` and flags work that remains `pending` or `running` past that duration.
Non-positive values select those defaults. `cocurrencyNum` is intentionally
spelled as shown and limits actual executor work shared by cron, manual runs,
and bulk retries; excess work waits as `pending`. Duplicate active work for the
same repository is rejected.
At startup the server reconciles executions left `pending` or `running` by the
previous process to `failed`, including active component rows.

## Security notes

- **Auth is off by default.** With `authEnabled: false`, anyone who can reach the server can trigger archive jobs, view logs, and modify your repository/storage configuration. Only run with auth disabled on a trusted network.
- **Use a strong `authToken`.** When `authEnabled: true`, set a long, random token. Clients send it as `Authorization: Bearer <token>` on every API request. See the [API reference](api.md#authentication) for details.
- **Bind carefully.** The default `host: localhost` only accepts connections from the same machine. To expose the server to other machines, set `host: 0.0.0.0` (or a specific interface) and put it behind a reverse proxy with TLS.
- **No TLS built in.** The server listens over plain HTTP. For remote access, terminate TLS at a reverse proxy (nginx, Caddy, etc.) in front of gitrieve.
- **Config file writes.** The server writes back to `config.yaml` when you edit repositories or storage through the UI/API. Ensure the file is writable by the user running gitrieve, and protect the file because it may contain storage credentials and tokens.

## Next steps

- [API reference](api.md) — for scripting or integrating gitrieve into automation.
- [README](../README.md) — for the CLI commands (`run`, `repository`, `release`, `daemon`) and the full configuration schema.
