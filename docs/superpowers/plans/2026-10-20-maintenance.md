# Safe maintenance implementation plan

> Execute with subagent-driven-development and test-driven-development. User approved the design in this conversation; no additional approval gate is needed.

**Goal:** Safely stop server work before Docker upgrades; restart with normal scheduling and no persistent resume queue.

**Architecture:** A context-carried maintenance signal is separate from hard cancellation. Executor closes admission, terminalizes pending work, and requests safe boundaries from running components. Status reports readiness only after admission, workers, and persistence have settled. Server APIs and a global UI control expose entry/status/exit; SIGTERM uses the same drain before HTTP/database shutdown.

**Tech stack:** Go, SQLite, Gin, gocron, plain JavaScript.

**Spec:** Approved conversation: Git network work may be interrupted; Issue/Discussion finish the current list page including all comments/replies then archive; Release finishes downloading and storing the current file. No cross-restart maintenance state, queue, or checkpoints. Future cron invocations create new executions. Fix Discussion ascending updated-time order first.

## Global constraints
- Preserve deletion-safe Git history and per-component cross-process locks.
- Repositories refresh only on explicit user action. Maintenance status updates must never refresh repository data.
- No task marked complete merely because maintenance interrupted it. Ordinary storage errors remain failures.
- Partial Issue/Discussion data must reach archive storage on soft stop. Without new remote updates, retry must still repair a previously failed archive publication.
- Maintenance mode resets on process restart. Historical cancellations remain in the database.

## Task 1: Component safe boundaries and Discussion ordering
Files: internal/maintenance/signal.go and tests; internal/{discussion,issue,release,repository} implementation and tests.
Interface: `NewSignal() *Signal`, `(*Signal).Request()`, `(*Signal).Done() <-chan struct{}`, `WithSignal(context.Context,*Signal) context.Context`, `Requested(context.Context) bool`, `ErrRequested` sentinel in package maintenance.
- [x] Add regression tests for ascending Discussion GraphQL queries, page-boundary stop plus archive publication, Release asset boundary, and Git interruptibility.
- [x] Run focused tests and observe expected failures.
- [x] Implement ascending ordering and soft stops. Check signal before starting work and at page/file boundaries; never abort the in-progress page/file because of soft stop. Finish archive writes before returning ErrRequested. Hard context cancellation retains its original behavior.
- [x] Ensure interrupted archive publication can be retried even if cache contains no newly updated records. Preserve current caches and previous archives; do not delete existing Git history.
- [x] Run focused package tests and report evidence.

## Task 2: Executor admission and draining
Files: internal/executor/maintenance.go, maintenance_test.go, executor.go.
Interface: `EnterMaintenance() MaintenanceStatus`, `MaintenanceStatus() MaintenanceStatus`, `ExitMaintenance() error`, `WaitForMaintenance(context.Context) error`; status JSON fields enabled, ready, running, pending, preparing. ErrMaintenance, ErrMaintenanceDraining.
- [x] Add blocking-runner and blocked-admission tests: no new dispatch after entry, soft signal does not cancel page context, readiness waits for persistence/admission, exit doesn't restart old jobs, fresh executor normal.
- [x] Implement state under queueMu, gate expansion/reservation/publication, cancel queued work with maintenance reason, and signal running jobs. Preserve failures and completed components.
- [x] Run executor and concurrency regression tests.

## Task 3: API, UI, shutdown and documentation
Files: internal/server/maintenance.go and tests, api.go; cmd/server/server.go and tests; web/templates/index.html, web/static/js/main.js, web/static/css/main.css, web/main.test.cjs; docker-compose.yml; README.md, README_zh.md.
- [x] Add API tests for authenticated maintenance status/entry/exit, job rejection, resume while draining, and shutdown waiting.
- [x] Expose GET/POST/DELETE /api/maintenance within existing auth group; reject single/bulk submissions in maintenance with HTTP 503, preserving partial bulk accounting if entry races a bulk request.
- [x] Add global status/button with explicit status refresh and safe upgrade readiness; no repository auto reload. Disable leave until drain finishes.
- [x] Handle SIGINT/SIGTERM with drain then HTTP shutdown then executor/database close; configure Docker stop grace and explain waiting for ready before upgrade.
- [x] Run full Go and web suites, build, relevant race checks where toolchain supports them, and review the complete diff.

## Decisions
- Pending executions are cancelled on maintenance entry; neither exit nor restart silently requeues them. This implements the approved next-schedule behavior without durable task state.
- Leaving maintenance is allowed only after drain completes; it reopens admission, without forcing a sync.
- UI may poll only the lightweight maintenance endpoint while draining; repository lists remain manually refreshed.

## Verification results

- Full `go test ./... -count=1`: passed after both review corrections.
- `node --test web/main.test.cjs`: 5 passed.
- Windows build and Linux amd64 cross-build: passed.
- `git diff --check`: passed.
- Race detector unavailable in this environment (`CGO_ENABLED=0`, no GCC found).
- Review corrections: preserve unresolved component persistence failures; never report safe readiness after failed drain persistence; republish cached Git snapshots after interrupted publication; keep complete no-cache archives and publish incomplete downloads under unique partial filenames.
