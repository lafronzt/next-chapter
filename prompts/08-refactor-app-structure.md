# 08 · Split the oversized files and tidy templates

## Context
The Go rebuild is compact but dense: `internal/app/routes.go` is 620 lines and `server.go` 410, with all HTTP handlers as methods on `*App`; `app_test.go` is one 412-line file; and most templates are written on one or two very long lines (see `base.html`, `index.html`, `plan.html`). Several literal values are repeated.

## Problems found (verified at `4bafb11`)
- Hard to navigate, review, or test handler groups in isolation. `server.go` mixes config, template funcs, session crypto, the middleware chain, rendering, and the login/index handlers. `routes.go` mixes form validation, plan storage, books, loans, plans/undo, imports, settings, sync, history, export, backup, and the demo.
- `New` contains a 40-line inline `template.FuncMap`.
- Validation (`bookFields`, the preferences/connection parsing in `settings`) is interleaved with request handling.
- The version appears in three places and has already drifted: `base.html` ("v0.2"), `cmd/next-chapter/main.go` ("Next Chapter 0.2.0 (Go)"), and the User-Agent in `sync.go` ("NextChapter/0.2"), plus the image tag in `deploy/kubernetes/app.yaml`.
- Templates already share a `fields` template for the hidden `csrf` and `revision` inputs, but some forms still repeat the hidden `csrf` input by hand (for example logout in `base.html`).

## Task (behavior must not change)
1. Split by responsibility **within the same package** (no new packages or exported API; handlers stay `func(w, r) error`):
   - `server.go`: `Config`, `App`, `New`, `Handler`, routing table.
   - `session.go`: session signing/reading, CSRF.
   - `middleware.go` (or inside `server.go` if it stays small): the outer handler pieces (headers, auth redirect, revision, form parsing).
   - `render.go`: `render`, `fail`, template funcs, notices map.
   - `handlers_auth.go`, `handlers_books.go` (index, new/edit, actions, loans), `handlers_plans.go` (suggest, plan, undo), `handlers_imports.go` (import page, examples, sync), `handlers_settings.go`, `handlers_export.go` (history, export, backup), `handlers_demo.go`.
   - `forms.go`: `bookFields`, preference and connection parsing as small functions returning typed values or `bad(...)` errors.
2. Add `var Version = "dev"` (or a `version.go` constant) as the **single source**: set it with `-ldflags "-X …/internal/app.Version=$VERSION"` in the Dockerfile (build arg, default from the tag) and use it in `main.go version`, the User-Agent, and a template function used by `base.html`'s edition label. Do not change the kubernetes tag text; just note in the report where else it must be bumped.
3. Reformat templates so each block, form, and tag group sits on its own lines with consistent indentation. Use `{{template "fields" .}}` everywhere the hidden `csrf`/`revision` pair is needed, and add a tiny `csrf` template for forms that need only the token. Do not change rendered markup beyond whitespace.
4. Move the large `FuncMap` into `render.go` as a package-level function.

## Acceptance criteria
- All existing tests pass **unmodified**. That is the primary proof that behavior is unchanged. If a test needs an edit, make the smallest one and say so.
- A rendered-HTML comparison: before refactoring, write a throwaway Go test (do not commit it, or commit it clearly as a golden test if you prefer) that seeds a demo library with the demo records plus a loan and a plan preview, fetches every page (`/`, each `view`, `/books/new`, `/books/1`, `/history`, `/imports`, `/settings`, `/login`, an error page, a plan preview) through `httptest`, and saves the bodies. After the refactor, normalize whitespace between tags (`>\s+<` → `><`, trim) and show the output is identical. Describe how you did it.
- No Go file over ~250 lines (excluding tests and CSS).
- `go vet ./...` and `gofmt -l .` clean.

## Notes
Do this before prompt 07 if you want smaller, more reviewable diffs there. Split `app_test.go` in prompt 10, not here, to keep this diff reviewable.
