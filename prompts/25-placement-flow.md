# 25 · Turn "Needs placement" into a short sorting session

## Context
New books join the end of the queue marked `NeedsPlacement` and the only cue is a small badge on the row (`index.html`). Placement is the app's central promise (suggestions never silently reorder), but after a Goodreads import with 30 new books the reader faces 30 badges and has to hunt. The pieces exist: `/suggest` (`suggest` in `handlers_*`, `propose.go`) finds the best slot for one candidate and shows a preview plan; the `action=placed` ("Keep here") button clears the flag; `moved_by_hand` and `pinned` constrain slots.

## Goal
One screen at a time, one book, a clear decision. No book moves until the reader confirms.

## Do
1. **Banner** at the top of the queue (and on the import-complete notice): "12 books need a spot. Place them →", linking to `/place` (new GET route; the next unplaced book by queue order).
2. **`/place` page** shows one book (cover tile, author, pages, tags, want-to-read pips, estimate if saved), the proposed slot from the same engine as `/suggest` (show the two neighbors above and below it, and the reasons), and three buttons: **Put it here** (applies the same reorder plan the suggest preview applies, with the same revision check and undo), **Keep it at the end** (`placed`), and **Decide later** (skip: moves to the next book without changing anything, and the book stays flagged).
3. After each decision go straight to the next unplaced book; when none remain, return to the queue with a notice naming how many were placed, moved, and skipped.
4. A progress line ("3 of 12") and a way out ("Back to my queue") on every step. Pinned books and series order stay enforced by the existing engine; do not reimplement ranking in the handler.
5. Re-use plans (`savePlan`/the `reorder` kind) rather than writing a second path, so previews, stale-revision handling, and Undo behave identically.
6. The AI is not involved here (it stays opt-in via prompt 16).

## Tests
- Placing N books one by one yields the same final order as applying the same N `/suggest` plans in the same sequence.
- "Decide later" changes no positions and leaves `needs_placement` set; the page then offers the next book, and after all are skipped the banner remains.
- A stale revision (library changed in another tab) shows the existing conflict message and loses nothing.
- Pinned and series constraints hold in every proposed slot.

## Out of scope
Batch auto-placement, drag-and-drop, changing the ranking model.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
