# GitHub Account Pool Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Use different GitHub accounts concurrently for initial and incremental issue synchronization while honoring rate limits.

**Architecture:** Extend the stable arbiter with credential-local state and a bounded account-fallback helper. All synchronization clients select credentials at each API attempt; immutable execution snapshots own eligible account lists.

**Tech Stack:** Go 1.23, go-github/v56, githubv4, Viper, YAML, testify, httptest.

**Spec:** `docs/superpowers/specs/2026-10-12-github-account-pool-design.md`

## Global Constraints

- Preserve `githubToken` fallback, existing retry semantics, locks and pagination.
- `githubApiConcurrency` stays global; `githubAccountConcurrency` defaults to 2 and applies to named pool accounts.
- Primary pauses belong to credential and resource; secondary pauses remain process-wide.
- Configuration reload must not replace the stable arbiter or clear active limits.
- Secrets never appear in previews, logs, or new error messages.
- Tests run without real GitHub tokens or outbound API calls.

### Task 1: Credential-aware arbiter

**Files:** `internal/githubapi/coordinator.go`, new `internal/githubapi/accounts.go`, new `internal/githubapi/accounts_test.go`.

**Interfaces:** Produce `type Account struct { Name string; Token string }` with yaml/mapstructure/json tags; add `Config.AccountConcurrency uint`. Produce `type AccountPermit interface { Permit; Token() string }`, `AcquireAccount(ctx context.Context, resource string, accounts []Account, excluded map[string]bool) (AccountPermit, error)` and `Do(ctx context.Context, resource string, accounts []Account, fn func(string) (Observation, error)) error`. Preserve existing `Acquire`, `Configure`, `Publish`, scope and publication interfaces.

- [ ] Write failing tests with literal credentials `a`, `b`: exhaust `a` core quota for an hour and verify `b` is admitted immediately; hold one slot per account and verify a third acquisition cancels at its deadline.
- [ ] Run `go test ./internal/githubapi` and observe missing feature failures.
- [ ] Implement fair selection, global/account caps, per-token resource/pacing state, cancellation and idempotent permits. Keep the stable arbiter pointer. Deduplicate identical token strings and distinguish anonymous/legacy from named pool caps.
- [ ] Test `Do` against typed REST 401, permission 403/404, primary quota errors and secondary errors; verify bounded candidate attempts, selected-token callback results and cooldown isolation. Handle known GraphQL error forms with bounded fallback and conservative cooldown when exact reset data is absent.
- [ ] Verify focused tests and existing coordinator tests. Commit only Task 1 files and write the implementation report.

### Task 2: Configuration lifecycle and documentation

**Files:** `internal/config/config.go`, `internal/config/importexport.go`, their tests, `internal/server/config_api.go`, configuration API tests, `config/example.config.yaml`, `README.md`, `README_zh.md`.

**Interfaces:** Consume `githubapi.Account` and `Config.AccountConcurrency`. Add `Config.GitHubAccounts []githubapi.Account`, `Config.GitHubAccountConcurrency uint` and `GetGitHubAccountsContext(ctx context.Context) []githubapi.Account`. The getter returns a detached normalized pool, uses only the pool when nonempty, otherwise returns the legacy token with an empty name, or an empty slice for anonymous mode.

- [ ] Add failing tests: parse/export/save/reload a two-account YAML document; clone/execution snapshot mutation must not modify published account slices; blank names/tokens and duplicate names fail validation without revealing tokens.
- [ ] Run `go test ./internal/config ./internal/server` and confirm failures before implementation.
- [ ] Extend config defaults, cloning, policy conversion and save. Extend ExportConfig, import parsing/defaults/validation, diffGlobals and chosen apply globals. Treat githubAccounts as a secret-containing list and mask each token independently in preview.
- [ ] Verify import choices `existing`/`imported` for both new fields and legacy documents. Add documentation showing two different users, `githubApiConcurrency: 4`, `githubAccountConcurrency: 2`, `cocurrencyNum: 12`, and explain per-credential pacing, global caps, private repository access and pool precedence.
- [ ] Run focused tests, commit only Task 2 files and write the report.

### Task 3: Synchronization integrations and final validation

**Files:** `internal/issue/issue.go`, `internal/discussion/discussion.go`, `internal/scm/github/github.go`, `internal/wiki/wiki.go`, `internal/githubapi/accounts.go`, `internal/githubapi/accounts_test.go`, tests in those packages and any narrowly needed common client helpers/test utilities.

**Interfaces:** Consume `githubapi.Do` and `config.GetGitHubAccountsContext`. Wrap each existing retry callback in `Do`; callbacks return `(Observation, error)` and authenticate using the chosen token. Build a fresh REST client over a copied HTTP client before `WithAuthToken`, avoiding empty-token auth; preserve BaseURL and existing test seams. The pinned go-github/v56 WithAuthToken mutates shared HTTP transport, so it must not wrap a shared client directly. A small helper shared by consumers may live in internal/githubapi.

```go
err := retry.Do(ctx, config.GetRetryConfigContext(ctx), func() error {
    return githubapi.Do(ctx, "core", accounts, func(token string) (githubapi.Observation, error) {
        httpCopy := *baseClient.Client()
        client := github.NewClient(&httpCopy)
        client.BaseURL, client.UploadURL = baseClient.BaseURL, baseClient.UploadURL
        client.UserAgent = baseClient.UserAgent
        if token != "" { client = client.WithAuthToken(token) }
        response, err := performRequest(client)
        return githubapi.ObserveREST(response, err), err
    })
})
```

- [ ] Write local HTTP tests that fail while only the old token is used: two requests rotate credentials, an exhausted first credential falls back promptly, permission failures try eligible alternatives, cancellation emits no request. Include real Issue/comment and GraphQL query consumers with captured authorization/pagination.
- [ ] Preserve anonymous REST after an unauthenticated GraphQL 401: add a failing regression that a later anonymous REST Do and legacy Acquire remain usable, then only persist invalid-credential disablement when the permit token is nonempty. Nonempty invalid credentials must remain disabled.
- [ ] Add failing pool-exhaustion tests: A primary reset in an hour and B in ten seconds expose the ten-second retry deadline; A permission403 and B primary exhausted remain retryable and later succeed; all-permission errors retain the exact first error. Keep distinct-token attempts bounded. Preserve first error diagnostics and errors.Is identity using the existing normalized error pattern, while exposing earliest primary retry evidence. Verify actual outer retry timing with short reset fixtures; GraphQL primary cooldown must stay associated with its resource.
- [ ] Run focused tests and confirm failure before wiring each consumer.
- [ ] Convert every covered API attempt, preserving response-derived pagination and GraphQL rateLimit reporting. Ensure failed GraphQL attempts cannot leave partially decoded query state that leaks into the next credential attempt.
- [ ] Run `go test ./...`, race tests for the arbiter/config/clients if supported, and `node --test web/main.test.cjs web/tests/*.test.js` with shell-expanded file paths. Review diff for credential leakage, permit leaks, and missed single-token call sites.
- [ ] Commit only Task 3 files and write the report. Complete independent whole-branch review and address findings before delivery.
