# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`AGENTS.md` (repo guidelines) and `CONTRIBUTING.md` (schema-change and data rules) are authoritative; read them before non-trivial changes. This file adds commands and architecture.

## Commands

Go 1.27+, no C compiler, no frontend build step.

- Build: `go build -trimpath -o ./bin/next-chapter ./cmd/next-chapter`
- Run locally without login: `LOCAL_DEMO=1 ./bin/next-chapter` (serves `localhost:8000`, loopback only). Real runs need `APP_PASSWORD` and `APP_SECRET_KEY` (see `.env.example`).
- All tests: `go test -race ./...`
- One test: `go test -race ./internal/app -run TestName`
- CI also runs `gofmt -l .` (must be empty), `go vet ./...`, `staticcheck`, and `go mod tidy` (must leave `go.mod`/`go.sum` unchanged).
- Container smoke test: `docker build -t next-chapter:local .` then `scripts/smoke.sh`.

## Architecture

Single package `internal/app` (everything lives here; `cmd/next-chapter` is only the entrypoint and health check). Single-reader, single-replica SQLite app with server-rendered HTML templates and one CSS file, all embedded from `internal/app/web/` (`templates/*.html`, `static/app.css`, `static/app.js`).

- **Ranking is pure and deterministic**: `model.go` / `propose.go` compute placement and full-queue suggestions with no DB or HTTP access. Keep it that way. Suggestions are previews; `plans.go` applies them with a revision check (stale-preview protection) and undo snapshots. `reasons.go` only explains order, it never reorders.
- **Persistence**: `store.go`, `books.go`, `listing.go`, `records.go`, `plans.go`. A global `revision` in the `meta` table gates stale previews and undo.
- **Schema**: `schema.sql` is the frozen v1 baseline, never edit it. Changes are appended to `migrations` in `migrations.go` and run by `migrate.go` in one `BEGIN IMMEDIATE` transaction with a `<db>.pre-vN.bak` backup. A new migration also requires checking JSON restore (`restore.go`) and raising `maxRestoreSchema` (`TestRestoreSchemaVersionIsReviewed` fails until you do), plus an upgrade test and README rollback notes.
- **Imports** (`imports.go`, `import_details.go`, `corrections.go`): Goodreads CSV, Libby timeline CSV, and a standard NYPL CSV. They must be transactional and idempotent. Matching order is Goodreads ID, ISBN, then normalized title+author. Imports append new books as "needs placement" and never reorder the queue.
- **Delete/merge** (`merge.go`, `aliases.go`): reader-initiated and previewed only. Each leaves a row in `book_aliases` that imports must keep honoring. Event fingerprints include book identity, so code that moves events between books must recompute them.
- **Separate streams that must not change status, pins, or order**: library events/loans, `reading_log` (a `source='manual'` row is never overwritten by import), reading progress, and progress checkpoints.
- **HTTP**: `server.go` (config from env and routes), `middleware.go`, `session.go`, `render.go`, `forms.go`, and `handlers_*.go` grouped by feature. POSTs carry CSRF tokens (`{{template "csrf"}}` / `"fields"` in templates).
- **Optional integrations, off by default**: book lookup (`lookup.go`) and AI assistance (`ai.go`, `jobs.go` for background requests). The app must work with neither, and must never require a paid service.

## Tests and UI

Tests are `internal/app/*_test.go`, using `httptest` and temporary SQLite databases with synthetic fixtures. `a11y_test.go` and `pwa_test.go` guard accessibility and mobile behavior. For UI changes, check desktop and narrow phone layouts (mobile rules are in `@media(max-width:760px)` blocks in `app.css`; later rules in the file override earlier ones) in light and dark themes.

`prompts/` holds numbered build prompts; `prompts/README.md` tracks which are implemented.
