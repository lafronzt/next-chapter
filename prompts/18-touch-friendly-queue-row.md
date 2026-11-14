# 18 · Touch-friendly queue rows with an action sheet

## Context
`templates/index.html` renders each queued book with up to seven controls in the row: "Find its place", ↑, ↓, Pin/Unpin, "Keep here", a "Move to #" number box with a Move button, and "Predict my rating". On a 390px screen the row is tall, several tap targets are small, and the primary information (title, author, position) is crowded out. The coarse-pointer block in `app.css` (~line 1398) only enlarges things.

## Goal
On every screen size the row shows the book and **one** primary action; everything else lives in an action sheet opened from a "⋯" button. Reordering with ↑ ↓ stays on the row, because the reader reorders often.

## Do
1. Row (to-read books): position, cover tile, title, author, badges, ↑ and ↓ (minimum 44×44px targets, with clear disabled state and the existing `title` explanation moved into visible text in the sheet), and a "⋯" More button.
2. Action sheet, built without JavaScript: a `<details>` per row (or a `:target` panel) that is a bottom sheet on narrow screens and a popover on wide ones. It holds Find its place, Pin/Unpin, Keep here, Move to #, Predict my rating, and Edit book, each as a full-width labelled button with a one-line explanation where the rule is non-obvious ("Pinned books keep their place"). Closing it must restore focus to the trigger.
3. Keep every action a real `<form method="post">` to the existing handlers (`/books/{id}/action`, `/suggest`, `/books/{id}/estimate`). No handler change is expected.
4. Rows for unreviewed books keep Want / Read / Not interested, but as three equal full-width buttons on narrow screens.
5. Make sure the long-title case wraps cleanly (a 120-character title with a subtitle) and the row never scrolls horizontally.
6. After an action the response is a redirect to the same page; keep the reader's place (see prompt 23 on scroll position) and the `return` field.

## Tests
- Update `web_test.go` / `a11y_test.go` for the new structure: the sheet's buttons keep their accessible names, each form still posts the right fields, and disabled reasons are present as text.
- Regression: moving, pinning, and "Move to #" still produce the same queue order and `moved_by_hand` flags as before.

## Out of scope
Drag-and-drop reordering, swipe gestures, any change to ranking.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
