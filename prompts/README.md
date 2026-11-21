# Prompts

Self-contained prompts for fixing and improving Next Chapter. Each file can be pasted into a fresh Claude Code session started in the repo root.

## Journals, quotes, and series discovery (48–49)

- [48 · Journal entries and quotes between Next Chapter and Hardcover](48-hardcover-journals-and-quotes.md) extends existing explicit note sharing with local quotes, verified inbound/outbound journal actions, privacy previews, stable identity, and conflict handling.
- [49 · Find new books in followed series](49-series-new-book-discovery.md) extends the series dashboard and catalog cache with followed series, new-member discovery, upcoming releases where supported, and previewed additions.

48 is a prompt only on this branch (see its own PR). 49 is implemented: following series by Hardcover id, a reader-initiated paginated check with first-check baselines, previewed additions through the existing series plan, dismissals, export/restore, and Forget. Release-date precision and a live-account check are not verified. Both require current API research and preserve reader-initiated changes, and can be implemented independently.

## Goodreads library export (47)

[47 · Export the library to Goodreads CSV](47-goodreads-library-export.md) adds a manual, read-only download alongside existing backups. Read the companion [research guide](47-goodreads-library-export-research.md) first to resolve ISBN eligibility, shelf mappings, rating precision, dates, and remote overwrite behavior. Status: implemented (`goodreads_export.go`). Goodreads' help pages need a login, so accepted rating precision, blank-value overwrite, and review HTML rules are unverified and handled conservatively; see the README section.

They were first written against the Python/Flask version (commit `c54c785`) and have been re-validated against the Go rebuild at commit `4bafb11` ("Replace Python runtime with self-contained Go application"). The Python code no longer exists, so every reference below is to the Go code under `internal/app/`. Baseline: 8 Go tests passing, 72.1% statement coverage (`go test -cover ./internal/app`).

## Suggested order

| # | Prompt | Type | Status after the Go rebuild |
|---|--------|------|-----------------------------|
| 05 | [Schema migration framework](05-schema-migrations.md) | Foundation | Done. |
| 02 | [Harden errors, login, and sessions](02-harden-errors-login-sessions.md) | Security | Done. |
| 06 | [Make the poller and sync resilient](06-poller-and-sync-resilience.md) | Bug | **Obsolete.** Goodreads turned off its feeds in 2026 and the RSS poller (`feed.go`, `sync.go`) was removed; CSV re-import is the only Goodreads path. Do not run this one |
| 03 | [Surface and resolve unreviewed books](03-unreviewed-books-workflow.md) | Bug + UX | Done. |
| 04 | [Delete and merge books](04-delete-and-merge-books.md) | Feature | Done. |
| 12 | [Goodreads ratings and JSON restore](12-ratings-and-json-restore.md) | Feature | Done. |
| 07 | [Pagination and large-library scaling](07-pagination-and-scaling.md) | Performance | Done. |
| 10 | [Expand tests](10-expand-tests.md) | Quality | Done. |
| 08 | [Split the oversized files and tidy templates](08-refactor-app-structure.md) | Refactor | Done. |
| 09 | [Accessibility and mobile pass](09-accessibility-and-mobile.md) | UX | Done. |
| 11 | [CI and supply-chain hygiene](11-ci-and-supply-chain.md) | Tooling | Done. |
| 13 | [Track reading history from Goodreads](13-reading-history-sync.md) | Feature (roadmap) | Done, via CSV import only (the feed half was removed). Ratings are quarter stars; a re-import moves queued books forward to Reading or Read |
| 14 | [Opt-in AI provider layer](14-ai-provider-layer.md) | Feature (roadmap) | Done. Claude or OpenAI, bring-your-own-key, off by default; changes the README privacy promise |
| 15 | [Estimated rating](15-ai-estimated-rating.md) | Feature (roadmap) | Done. Deterministic baseline plus optional AI refinement; needs 13 and 14 |
| 16 | [AI-assisted reordering](16-ai-assisted-reordering.md) | Feature (roadmap) | Done. AI proposes, rules enforce pins and series, reader confirms; needs 14 |
| 17 | [Installable web app](17-installable-web-app.md) | UI (roadmap) | Done. Manifest, icons, theme color, safe areas; no service worker |
| 18 | [Touch-friendly queue rows](18-touch-friendly-queue-row.md) | UI (roadmap) | Done. One primary action per row, the rest in a no-JS action sheet |
| 19 | [Mobile bottom navigation](19-mobile-navigation.md) | UI (roadmap) | Done. Fixed bottom tab bar with Add and More; sidebar stays on desktop |
| 20 | [Next-up home screen](20-next-up-home.md) | UI (roadmap) | Done. Next up, Reading now, Due soon, and a quick Finished flow |
| 21 | [Quarter-star rating input](21-quarter-star-rating-input.md) | UI (roadmap) | Done. Replaces the 20-option select; needs the quarter-star migration (done) |
| 22 | [CSV-first import page](22-csv-first-import-page.md) | UI (roadmap) | Done. Goodreads export guide, summary-first preview, last-imported status |
| 23 | [UI polish](23-ui-polish-dark-mode-and-search.md) | UI (roadmap) | Done. Dark mode, sticky search, filters and sort, place-keeping redirects, collapsed book form |
| 24 | [Vocabulary: Interest vs Rating](24-vocabulary-interest-vs-rating.md) | UI (roadmap) | Done. "Want-to-read" pips vs star ratings, one status vocabulary, stale feed wording |
| 25 | [Placement flow](25-placement-flow.md) | UI (roadmap) | Done. One book at a time through "Needs placement", via the existing suggest/plan path |
| 26 | [Queue reasons](26-queue-reasons.md) | UI (roadmap) | Done. One quiet reason per row; hand-placed books look different. Overlaps 18 (the row) |
| 27 | [Book view and edit pages](27-book-view-and-edit.md) | UI (roadmap) | Done. Read-only reading page plus `/books/{id}/edit`. Do before 20's Finished flow lands on the book page |
| 28 | [Visual direction](28-visual-direction.md) | UI (roadmap) | Done. 16px body, serif titles, hashed cover tiles, fewer eyebrows. Share the palette with 23 |
| 29 | [Reading history page](29-reading-history-page.md) | UI (roadmap) | Done. Year sections, quarter-star distribution, filters |
| 30 | [Notices, Undo, empty states](30-specific-notices-undo-and-empty-states.md) | UI (roadmap) | Done. Specific notices, undo for moves, pins and status changes |
| 31 | [Grouped Preferences](31-grouped-preferences.md) | UI (roadmap) | Done. Ranking, Privacy, AI sections that save independently |
| 32 | [Book details lookup and descriptions](32-book-details-lookup-and-descriptions.md) | Feature (roadmap) | Done. Opt-in Open Library / Google Books lookup, `books.description` (migration 10), the estimate uses the description. Companion: [external-agent-book-lookup.md](external-agent-book-lookup.md) |
| 01 | [Insertion ranking performance](01-fix-insertion-performance.md) | Optimization | Done. |

**Roadmap track (13 → 14 → 15 → 16):** history sync, then the AI layer, then the two AI features. Build it on the fixes first: at least 05, 02, 06, and 12A. 13 is useful on its own with no AI. 14–16 deliberately relax the README's "no AI provider receives reading data" promise, so they are strictly opt-in, bring-your-own-key, preview-before-send, and propose-only (AI never writes order or ratings directly; the reader confirms through the existing plan flow). Pins and series constraints remain deterministic.

**UI track (17 → 23):** a phone-first pass. Order: 19 and 18 first (they fix the daily pain), then 21 (needed by 20), 20, 22, then 17 and 23 in either order. 09 is the older accessibility audit; do it first if you want its findings fixed before restructuring templates. **Second UI pass (24 → 31):** design and flow, after the phone-first pass. Order: 24 (vocabulary) first because later prompts reuse its labels; then 27 and 25; then 26 (coordinate with 18, which owns the row layout); then 28, 29, 30, 31. 20, 22 and 27 all touch what happens when a book is started or finished, so land 27 before 20 or merge their Finished flow. All of the UI prompts keep the CSP (no inline script or style), the no-JavaScript baseline, and the full-page POST + CSRF + revision contract.

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
- **`importRecords` is shared** by CSV import, `newBook`, `loan`, and the demo. A change to its insert path affects all of them.
- **`plan()` dispatches on `kind`** with `if kind == "import" { … } else { reorder }`. A new plan kind must get its own branch or it will be applied as a reorder.

## Additional workflow prompts (33–38)

These prompts extend the implemented Go app. They can be used independently; inspect current code rather than relying on the older status table above. Apply schema migrations using the next available version at implementation time.

| # | Prompt | Scope | Status |
|---|--------|-------|--------|
| 33 | [Private reading notes](33-private-reading-notes.md) | Reasons for adding books and dated private notes, excluded from AI | Done |
| 34 | [Saved library views](34-saved-library-views.md) | Structured filters, local ownership, and reusable saved views | Done |
| 35 | [Borrowing calendar](35-borrowing-calendar.md) | Current deadlines, estimated holds, and a private calendar download | Done |
| 36 | [Series dashboard](36-series-dashboard.md) | Local series progress and next known unread volumes | Done |
| 37 | [Reading sessions and pace](37-reading-sessions-and-pace.md) | Optional dated progress checkpoints and approximate finish dates | Done |
| 38 | [Import reconciliation and corrections](38-import-reconciliation-and-corrections.md) | Match explanations, durable event corrections, and alias management | Done |

Suggested order: 33 → 34 → 36 → 35 → 37 → 38. Calendar and series views can also be built first without the other additions. Keep 38's three parts as focused implementation steps. These prompts do not authorize external messaging or automatic library-account access.

## Hardcover integration (39–46)

Two-way integration with [Hardcover](https://hardcover.app) through its GraphQL API (`docs.hardcover.app/api`). Optional, off by default, bring-your-own token, every sync reader-initiated and previewed. The API is in beta, so 39 verifies the schema at connect time. Hardcover must never be required, and these prompts never touch other users' data, reviews, or private notes. Apply migrations using the next available version at implementation time.

| # | Prompt | Scope | Status |
|---|--------|-------|--------|
| 39 | [Connection and API client](39-hardcover-connection.md) | Env token, rate-limited client, status page, privacy wording | Foundation implemented on `feat/hardcover-integration-39-46`; synthetic-server coverage, live authenticated schema capture pending |
| 40 | [Book linking](40-hardcover-book-linking.md) | Reader-confirmed `hardcover_book_id` links by ISBN, then title+author | Implemented on `feat/hardcover-40-book-linking`; synthetic-server coverage, live API check pending |
| 41 | [Pull library](41-hardcover-pull-library.md) | Import shelves, ratings, finish dates as a previewed, idempotent import | Implemented on `feat/hardcover-41-pull-library`; synthetic-server coverage, live API check pending |
| 42 | [Push status and ratings](42-hardcover-push-status-and-ratings.md) | Previewed outbound updates, conflict handling, private by default | Implemented on `feat/hardcover-42-push-status-and-ratings`; synthetic-server coverage, request shapes taken from the official SDK, live API check pending |
| 43 | [Progress and read dates](43-hardcover-progress-and-dates.md) | Two-way current-read progress, re-reads, date precision | Implemented on `feat/hardcover-43-progress-and-dates`; synthetic-server coverage, request shapes from the official SDK, live API check pending |
| 44 | [Catalog details and series](44-hardcover-catalog-details-and-series.md) | Hardcover as a `BookSource`, complete series, optional covers | Implemented on `feat/hardcover-44-catalog-details`; synthetic-server coverage, shapes from the official SDK, image host and live API unverified |
| 45 | [Sync center](45-hardcover-sync-center.md) | One page for changes in both directions, conflicts, history, disconnect | Implemented on `feat/hardcover-45-sync-center` (scheduled check not built); synthetic-server coverage, live API check pending |
| 46 | [Queue list and OAuth](46-hardcover-queue-list-and-oauth.md) | Optional: publish the queue as a private list; OAuth instead of a PAT | Not started |

Order: 39 → 40 → 41 → 42 → 43 → 44 → 45, with 44 able to follow 40 at any point. 46 is optional. 41 alone gives a one-way import that replaces the Goodreads CSV round trip.
