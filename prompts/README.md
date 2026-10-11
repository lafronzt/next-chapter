# Prompts

Self-contained prompts for fixing and improving Next Chapter. Each file can be pasted into a fresh Claude Code session started in the repo root.

They were first written against the Python/Flask version (commit `c54c785`) and have been re-validated against the Go rebuild at commit `4bafb11` ("Replace Python runtime with self-contained Go application"). The Python code no longer exists, so every reference below is to the Go code under `internal/app/`. Baseline: 8 Go tests passing, 72.1% statement coverage (`go test -cover ./internal/app`).

## Suggested order

| # | Prompt | Type | Status after the Go rebuild |
|---|--------|------|-----------------------------|
| 05 | [Schema migration framework](05-schema-migrations.md) | Foundation | **Raised priority.** `OpenStore` now runs an ad-hoc, unbacked-up `go_data_version` data rewrite on legacy databases; fold it into a real framework |
| 02 | [Harden errors, login, and sessions](02-harden-errors-login-sessions.md) | Security | Still open: global login lockout (and the existing test asserts it), stateless sessions, no request IDs, no HSTS. The `ValueError` problem is gone |
| 06 | [Make the poller and sync resilient](06-poller-and-sync-resilience.md) | Bug | Reshaped: thread-death bug is gone, but one error message covers every failure, redirects are always rejected, no backoff, no panic guard, and `FetchGoodreads` cannot be tested |
| 03 | [Surface and resolve unreviewed books](03-unreviewed-books-workflow.md) | Bug + UX | Unchanged in substance |
| 04 | [Delete and merge books](04-delete-and-merge-books.md) | Feature | Adjusted: queue positions are global, not queue-only |
| 12 | [Goodreads ratings and JSON restore](12-ratings-and-json-restore.md) | Feature | Adjusted: plan kinds, per-route upload limit, `interest=0` trap |
| 07 | [Pagination and large-library scaling](07-pagination-and-scaling.md) | Performance | Still open; the single DB connection makes slow requests block everything |
| 10 | [Expand tests](10-expand-tests.md) | Quality | Rewritten for `go test`: split the one file, add fuzz and property tests |
| 08 | [Split the oversized files and tidy templates](08-refactor-app-structure.md) | Refactor | Rewritten: a file split inside one Go package, golden-HTML check, one `Version` source |
| 09 | [Accessibility and mobile pass](09-accessibility-and-mobile.md) | UX | Same findings, verified against the Go templates |
| 11 | [CI and supply-chain hygiene](11-ci-and-supply-chain.md) | Tooling | Rewritten for Go: SHA pins, lint, `govulncheck`, digest-pinned base images, `ldflags` version |
| 13 | [Track reading history from Goodreads](13-reading-history-sync.md) | Feature (roadmap) | New. Finish dates, ratings, and a history page; needs 05 and 12A |
| 14 | [Opt-in AI provider layer](14-ai-provider-layer.md) | Feature (roadmap) | New. Claude or OpenAI, bring-your-own-key, off by default; changes the README privacy promise |
| 15 | [Estimated rating](15-ai-estimated-rating.md) | Feature (roadmap) | New. Deterministic baseline plus optional AI refinement; needs 13 and 14 |
| 16 | [AI-assisted reordering](16-ai-assisted-reordering.md) | Feature (roadmap) | New. AI proposes, rules enforce pins and series, reader confirms; needs 14 |
| 01 | [Insertion ranking performance](01-fix-insertion-performance.md) | Optimization | **Downgraded.** Go is O(n²), not cubic: 313 ms at 2,000 books. Worth doing, not urgent |

**Roadmap track (13 → 14 → 15 → 16):** history sync, then the AI layer, then the two AI features. Build it on the fixes first: at least 05, 02, 06, and 12A. 13 is useful on its own with no AI. 14–16 deliberately relax the README's "no AI provider receives reading data" promise, so they are strictly opt-in, bring-your-own-key, preview-before-send, and propose-only (AI never writes order or ratings directly; the reader confirms through the existing plan flow). Pins and series constraints remain deterministic.

Do 05 before any prompt that changes the schema (03, 04, 12, and the login table in 02). Do 10 alongside the others rather than last. Do 08 before 07 if you want smaller diffs. 01 can wait.

## Ground rules (apply to every prompt)

- Read `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first. Honor their invariants: never silently reorder a queue; preserve the separation between reading intent, queue order, and library events; imports are idempotent and transactional; a loan or return never implies completion.
- Verify with `go test -race ./...`, `go vet ./...`, and `gofmt -l .` (must print nothing). Go 1.27, `CGO_ENABLED=0`, pure-Go SQLite (`modernc.org/sqlite`).
- Add regression tests for every behavior change, using synthetic fixtures only. Tests live in package `app` and use the helpers in `internal/app/app_test.go` (`testApp`, `browser`, `addRecords`, `records`). Prefer the standard library: no new test framework.
- No new mandatory paid services, no frontend build step, and minimal new dependencies (standard library first). Justify any addition in the report.
- Keep the diff focused. Match the existing style (handlers are `func(w, r) error`; errors for the user are `bad()`/`conflict()`; data access goes through `queryer`). Update the README only where documented behavior changed.
- Finish by reporting what changed, the test results, and anything you chose not to do.

### Go-specific traps (read before touching `store.go` or handlers)

- **One database connection.** `OpenStore` sets `SetMaxOpenConns(1)`. Every request, including `/healthz` and the per-request `revision()` read in the middleware, queues behind whatever holds that connection. Consequences: (a) slow work inside `Store.Write` stalls the whole app and can fail the container `HEALTHCHECK`; (b) never keep a `*sql.Rows` open on `store.DB` while issuing another query on `store.DB`, which deadlocks (inside a `*sql.Tx` it is safe). Existing code closes rows before the next query; keep doing that.
- **Transactions are deferred**, not `BEGIN IMMEDIATE`. In one process the single connection already serializes writers. Anything that must hold up against a second process (migrations) needs an explicit write lock.
- **Positions are global.** `books.position` is assigned `MAX(position)+1` over *all* books at insert, but `reorder()` rewrites only the `to-read` subset as `0..n-1`. So non-queue books can share position numbers with queue books, and ordering relies on `ORDER BY position, id`. Do not assume queue positions are contiguous across the whole table.
- **`importRecords` is shared** by CSV import, RSS sync, `newBook`, `loan`, and the demo. A change to its insert path affects all of them.
- **`plan()` dispatches on `kind`** with `if kind == "import" { … } else { reorder }`. A new plan kind must get its own branch or it will be applied as a reorder.
