# 20 · A home screen for "what do I read next?"

## Context
The home page (`/`, `templates/index.html`) is a paged list. The two questions the reader opens the app to answer are "what's next?" and "what's due back at the library?". Due dates exist only as small badges on rows (`activeLoan` in `index.html`), and finishing a book takes several clicks through the book form.

## Do
1. **Next up card** above the queue (first page, no search): the first `to-read` book that is not `NeedsPlacement`-blocked, shown large with cover tile, author, pages, why it ranks first (reuse the existing score reasons if they are available without recomputation, otherwise omit), and a **Start reading** button.
   - Start reading POSTs to the existing status path (`bookAction` or a new narrow action) and sets the status to `reading`. It must not reorder anything else, must clear the pin the same way `statusChange` does, and must be undoable or reversible from the book page.
2. **Reading now** strip: books with status `reading`, each with **Finished** and **Stop reading** actions.
3. **Due soon** strip: active Libby/NYPL loans due within 7 days, soonest first, with the due date and days left (`libraryStatus` in `rerank.go` already derives this; reuse it). Overdue is shown plainly, not alarmingly. Loans never imply status.
4. **Finished** opens a small form (or page) with a finish date defaulting to today and the quarter-star rating control from prompt 21; submitting it sets status `read` and logs a reading through the same code as `addReading`/`importRecords` (one reading per finish; fingerprint rules in `history.go` apply). A reading never changes interest or queue order.
5. Empty states: no reading → no strip; no loans → no strip. The page must still be fast: the new queries reuse the single DB connection, so close rows before issuing the next query, and add no per-row queries.

## Tests
- Start reading moves exactly one book out of the queue and changes no other position; Finished logs exactly one reading and is not duplicated on a double submit (CSRF + revision check).
- Due-soon lists only active loans inside the window, in order, and never changes a book's status.
- Unreviewed and archived books never appear in Next up.

## Out of scope
Notifications or reminders; AI "what should I read" picks (that is prompt 16).

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
