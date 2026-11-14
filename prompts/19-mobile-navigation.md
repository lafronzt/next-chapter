# 19 · Bottom navigation on phones

## Context
`templates/base.html` puts eight links in a left `<aside class="sidebar">`. At `max-width:760px` it becomes a static block above the content (`app.css` ~line 1186), so on a phone the reader scrolls past the whole menu on every page, and "Add a book" is reachable only from empty states and `/books/new`.

## Goal
On narrow screens: a fixed bottom tab bar with the five things used daily; the sidebar stays as is on desktop.

## Do
1. Bottom bar (`<nav aria-label="Main">`) at `max-width:760px`: **Queue**, **To review** (with the unreviewed count badge), **Add** (a prominent centered button to `/books/new`), **Reading** (history), **More**. Use `aria-current` for the active tab, 48px-high targets, and `env(safe-area-inset-bottom)` padding.
2. "More" opens a page or `<details>` panel with All books, Library history, Import books, Preferences, and Sign out. It must work without JavaScript.
3. Hide the sidebar's link list on narrow screens (keep the brand), and remove the duplicate topline and footer copy there. Give `main` bottom padding so the bar never covers the last row or a form's submit button.
4. Keep exactly one `<nav>` landmark exposed to assistive tech at a time (hide the other with `display:none`, not just visually), and keep the skip link.
5. Nothing may sit under the on-screen keyboard: forms should still be usable when a field is focused (the bar should not jump over inputs; hide or un-fix it while a form field has focus, using `:has(input:focus)` where supported).

## Tests
- `a11y_test.go`: only one visible main navigation landmark per width is not testable from Go, so assert both navs have distinct `aria-label`s and every destination appears in at least one. Add a web test that "To review" count and `aria-current` render on the right tab for each of `/`, `/?view=unreviewed`, `/reading`, `/books/new`.

## Out of scope
Changing routes or page titles.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
