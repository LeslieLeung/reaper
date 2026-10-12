# GitHub account pool

The user approved a shared account pool on 2026-10-12 to accelerate initial issue synchronization across more than 6,000 repositories.

## Configuration and compatibility

Add `githubAccounts`, a list of `{name, token}` entries, and `githubAccountConcurrency` (default 2). An explicit nonempty account list takes precedence over `githubToken`; otherwise the existing token or anonymous REST mode remains available. Account names and tokens must be nonblank, names must be unique, and exact duplicate tokens must not multiply concurrency or quota. Tokens belonging to the same GitHub user still share GitHub quota; document the requirement to use different users.

`githubApiConcurrency` remains the process-wide in-flight limit. Named pool accounts have their additional per-account cap. Legacy authentication keeps its previous concurrency behavior. `githubMinRequestInterval` applies to starts for each credential. REST and GraphQL share each account's cap and pacing; their primary quota pauses remain separate. A secondary-limit pause remains process-wide and cannot be bypassed by rotation or reload.

All configuration paths support these fields: initial load, save, export, parse/validate import, import diff/apply, reload, execution snapshots. Import previews retain account names but mask all tokens. Configuration copies own their account slices. No credentials are printed in logs or error messages.

## Stable arbiter and selection

Extend the stable process arbiter with per-credential state. Pick available accounts fairly, skipping busy, paced, quota-paused and disabled accounts. Selection plus global/per-account slot reservation is atomic. Cancellation must interrupt waits, including when every account is cooling down, and cancelled contexts must not send HTTP requests. Releasing a permit is idempotent. Configuration changes retain outstanding permits, per-credential quota/pacing/disabled state and global secondary pauses; old and new execution snapshots share the same arbiter while retaining their own eligible credentials.

Expose `githubapi.Account` with `Name` and `Token`, `AcquireAccount(ctx, resource, accounts, excluded)` returning a token-bearing permit, and `githubapi.Do(ctx, resource, accounts, fn)` around one network attempt. Preserve `Acquire` for existing consumers/tests. Callers supply immutable execution-snapshot credentials. `Do` reports the response to the selected account, immediately tries another eligible account for primary exhaustion or invalid credentials, and uses bounded per-call fallback for permission-related 403/404. A 401 disables only that credential in this process; changing its token allows recovery. Permission failures never disable an account globally and never become empty successful downloads. Secondary limits return to the existing retry policy without attempting another account. Generic transient errors retain the existing bounded retry policy. Exhausting candidates returns the original error rather than looping indefinitely.

## Integration

Issue pages and comment pages, Discussion queries/comments/replies, repository discovery, release metadata/assets and asset authorization use the shared selection helper. Keep pagination, retry counts, cache writes and repository locking unchanged. REST clients use an immutable authenticated copy for each selected token, preserving custom BaseURL settings. GraphQL creates an authenticated client for each selected credential; successful query quota belongs to that permit. Asset body transfers remain outside long-lived API permits once authorization returns.

## Verification

Tests cover fairness, duplicate tokens, global and per-account caps, per-account pacing, primary resource isolation, secondary pauses, invalid/permission fallback, bounded exhaustion, cancellation, idempotent release, and reload without resetting active quota state. Configuration tests cover all persistence/import paths and secret masking. Local HTTP integrations confirm that actual REST/GraphQL requests use selected tokens, maintain pagination and switch accounts without the exhausted account's reset sleep. Run `go test ./...`, relevant race tests when toolchain supports them, and existing web tests.
