# 30 · Specific notices, broader Undo, actionable empty states

## Context
Notices come from the `notices` map in `render.go` and are generic ("Saved to your library.", "Your new order is saved."). The only Undo (`undo_queue`, `handlers_plans.go`, a single-slot snapshot of queue order) covers reorder and insert plans, not status changes, pins, or single moves. Empty states read warmly but do not say what to do next ("Your next chapter starts here.").

## Do
1. **Specific notices.** Each action's redirect carries what happened: "Moved *The Open Era* to #3.", "Pinned *Less*.", "*Dracula* is now Reading.", "12 books added, 3 need a spot." Titles must be HTML-escaped when rendered and capped in length. Prefer short-lived server-side state (a signed value in the existing session cookie, or a query parameter holding only an ID and an action key, not free text) over putting titles in URLs, so notices cannot be forged into arbitrary page text. A forged or stale key shows the generic notice.
2. **Broader Undo.** Extend the single-slot undo to cover: Move up/down, Move to #, Pin/Unpin, "Keep here", and a status change from the queue row or book page. Reuse `undo_queue` and its revision guard (undo is refused once the library has changed again) rather than adding history. The notice shows an **Undo** button (a POST form) next to the message. An undo of a status change restores status, pin, `needs_placement`, `moved_by_hand`, and position.
3. **Empty states say the next step**, with one primary button each: empty queue → "Import your Goodreads CSV" or "Add a book"; empty To review → "Import library activity"; empty search → "Clear the search"; empty reading history → "Mark a book as finished".
4. Keep screen-reader behavior: notices are `role="status"`; the Undo button has an accessible name that includes the book title.

## Tests
- Each action's notice names the right book and position; titles with `<script>` and 500-character titles render safely and truncated.
- Undo round-trips each covered action to byte-identical book rows (compare `loadBooks` before and after); undo after an intervening change is refused with the existing conflict message and loses nothing.
- Forged notice parameters never render attacker text.

## Out of scope
Multi-level undo history, undoing imports (use the preview/confirm step), AI plans.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
