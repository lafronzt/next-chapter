# Repository Guidelines

## Project Structure & Module Organization

Next Chapter is a single-reader Go web app using SQLite, server-rendered HTML, and plain CSS. `cmd/next-chapter/` contains the executable and health check. `internal/app/` holds ranking (`model.go`, `propose.go`), parsing (`imports.go`), persistence (`store.go`, `books.go`, `listing.go`, `records.go`, `plans.go`, `schema.sql` as the frozen v1 baseline), schema migrations (`migrations.go`, applied by `migrate.go`), delete/merge and aliases (`merge.go`), RSS polling (`feed.go`, `sync.go`), and HTTP handling (`server.go` for config and routes, `middleware.go`, `session.go`, `render.go`, `forms.go`, and `handlers_*.go`). `version.go` holds the build-time version. Templates, static assets, and bundled CSV examples live in `internal/app/web/` and are embedded in the binary. Tests reside in `internal/app/*_test.go`. Root `examples/` contains import samples; `deploy/kubernetes/` and `.github/workflows/` contain deployment and CI configuration.

## Build, Test, and Development Commands

Use Go 1.27 or later. No frontend build step or C compiler is required.

- `go build -trimpath -o ./bin/next-chapter ./cmd/next-chapter`: build the executable.
- `LOCAL_DEMO=1 START_POLLER=0 ./bin/next-chapter`: run locally at `localhost:8000` without login or background polling; demo mode requires loopback binding.
- `go test -race ./...`: run all tests with race detection.
- `go vet ./...`: run Go static checks.
- `docker build -t next-chapter:local .`: build the container; CI also checks container health.

## Coding Style & Naming Conventions

Format Go changes with `gofmt`, using standard tab indentation. Follow existing Go naming: exported identifiers use PascalCase, internal identifiers use camelCase, and package names are lowercase. Keep ranking deterministic and separate from persistence and HTTP concerns. Extend the existing HTML templates and CSS for UI changes.

## Testing Guidelines

Use Go's `testing` package and `net/http/httptest`. Name files `*_test.go` and functions `TestBehavior`. Use temporary SQLite databases and synthetic or anonymized fixtures. Add regression tests for ranking constraints, identity matching, and import behavior. No numeric coverage threshold is configured. Run race tests and vet before submitting; check desktop and narrow mobile layouts for UI changes.

## Commit & Pull Request Guidelines

History uses descriptive imperative subjects, with an occasional `feat:` prefix; no strict prefix convention is established. Keep commits and PRs focused. Describe the workflow change, link relevant issues, and report validation. Include screenshots for visual changes. Follow `CONTRIBUTING.md`.

## Data & Security Rules

Never silently reorder queues or treat loans/returns as reading completion. Keep imports transactional and idempotent. Schema changes require explicit migrations and backup/rollback instructions. Never commit `.env`, credentials, personal exports, or database backups. Preserve single-replica SQLite operation and self-hosting without mandatory paid services.
