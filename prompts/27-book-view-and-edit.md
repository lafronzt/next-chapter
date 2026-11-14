# 27 · Split the book page into a reading page and an edit page

## Context
`GET /books/{id}` (`editBook`, `templates/book.html`) is the edit form followed by the estimate, readings, loans, and events, all stacked. Most visits are to look something up or act on status, not to edit fields, and the long scroll is worst on a phone.

## Do
1. **New view at `/books/{id}`** (read-only): cover tile, title, author, pages, tags, series, want-to-read pips, current status badge with the one obvious next action (queue → **Start reading**, reading → **Finished**, read → **Add another reading**, unreviewed → the three triage buttons, set aside → **Move back to my queue**), then sections: Your readings (with quarter-star ratings), Library (loans and recent events), Estimate. A single **Edit details** link goes to the form.
2. **Edit form moves to `/books/{id}/edit`** (GET and POST). Keep the existing handler logic and validation (`editBook` POST), the delete and merge links, and the stale-revision behavior. Redirect from POST to the view page. Old links to `/books/{id}` keep working (they now show the view); update every `href="/books/{{.ID}}"` that meant "edit" (row covers, "Edit book" buttons, `aria-label`s that say Edit) so labels match the destination.
3. Status actions on the view page reuse the existing POST handlers (`bookAction`, `triageBook`, the Finished flow from prompt 20 if it exists); do not add a second implementation. A status change never reorders other books and clears pins exactly as `statusChange` does.
4. New-book creation (`/books/new`) keeps using the form.
5. Sections load with the same few queries the page runs today; add none per row.

## Tests
- `/books/{id}` renders without form fields and `/books/{id}/edit` with them; POST to edit validates as before and redirects to the view; 404 for missing IDs on both.
- Each status shows exactly its own primary action, and each action's POST yields the same result as the matching button on the queue row.
- Update web, a11y, and merge/delete tests for the new URLs.

## Out of scope
Notes or journaling fields, cover image upload.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
