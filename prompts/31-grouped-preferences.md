# 31 · Group Preferences into Ranking, Privacy, and AI

## Context
`templates/settings.html` is one long page after the Goodreads connection section was removed: ranking sliders and selects, "Let my saved estimated ratings nudge suggestions", "Keep my Goodreads review text", a Save button, then a separate AI panel with its own forms (`/ai/settings`, `/ai/test`), a schema version, and a sessions area. It reads as a settings dump, and the nav label is still partly feed-era.

## Do
1. Three sections, each a `<details>` (open by default for Ranking, collapsed for the others unless the reader arrived via `#ai` or `#privacy`): **Ranking** (library timing, length, mood, use saved estimates), **Privacy** (keep review text, with the existing explanation that turning it off deletes stored text and a confirmation step for that destructive change; sign out of all devices if present), **AI assistance** (the existing panel, unchanged in behavior).
2. Each section saves independently with its own button and its own revision check so a reader cannot lose an AI change by saving a ranking change. The Ranking and Privacy forms both post to `/settings`; decide whether to split the route or add a hidden `section` field, and keep the validation messages and tests intact (`web_test.go` covers invalid preference values).
3. Add a small "About this library" footer (version, schema version, database path is **not** shown, export and backup links). Move the existing export/backup links here if they are elsewhere.
4. Each setting gets a one-sentence plain explanation under its label (no more than the current README says), and defaults are shown ("Default: Any").
5. Anchors work from other pages (`/settings#privacy`, `/settings#ai`) and from the bottom navigation's More panel (prompt 19).

## Tests
- Saving each section changes only its own keys in `preferences` and bumps the revision once; invalid values still return 400 with the same messages.
- Turning off review text deletes stored text only after confirmation and only in the Privacy save.
- The AI section behaves exactly as before (existing `ai_test.go`/web tests pass unchanged).

## Out of scope
New settings, account or password management UI.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
