# 23 · Polish: dark mode, search and filters, calmer copy

## Context
Smaller changes that make the app nicer day to day. None needs new data or routes beyond query parameters.

## Do
1. **Dark mode.** `app.css` has no `prefers-color-scheme` support. Move colors to CSS custom properties (some already exist), add a dark palette under `@media (prefers-color-scheme: dark)`, and make sure the cover tiles, badges, notices, focus rings, and the "dark intention panel" keep WCAG AA contrast (check the focus ring rule near line 42). No toggle needed; follow the system. Match `theme-color` from prompt 17.
2. **Sticky search and filters on the list pages.** Keep the search box visible while scrolling on narrow screens. On All books add a status filter (to-read, reading, read, archived, unreviewed) and a sort (queue order, title, author, date added, interest) as query parameters handled in `listing.go`; the default stays queue order and a changed sort must never touch stored positions (the queue order is not a sort preference). Keep pagination links carrying the filters.
3. **Keep the reader's place.** After a Move, Pin, or triage action the page reloads at the top. Redirect with a fragment to the affected row (`#book-123`) and add `id`s and `scroll-margin-top` so the row lands in view, without JavaScript.
4. **Collapse the book form.** `templates/book.html` shows everything at once. Put Title, Author, Status, and Interest first; move Tags, Series, ISBN, Pages, Published year, and the Goodreads ID into a "Details" `<details>` that is open when any of those has a value.
5. **Calmer copy on small screens.** Hide the decorative strings ("A little intention. A better next read.", the "MAKE ROOM FOR YOUR NEXT GOOD BOOK" topline, the footer tagline) at `max-width:760px`; keep them on wide screens if you like them.
6. **Loading and error states.** Disable a submit button after the first click (CSS-only is not possible; use a tiny `app.js` enhancement, or skip it and say so), and make the AI "working" notice readable on mobile.

## Tests
- Sorting and filtering return the expected rows, preserve pagination, and leave `books.position` untouched.
- Redirect fragments point at an existing row id; the id format is unique per row.
- A contrast check script is not required in CI, but report the ratios you measured for the dark palette.

## Out of scope
A theme toggle, custom themes, real cover images (they would need an outbound request, which breaks the no-external-service rule).

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
