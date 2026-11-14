# 24 · One word, one meaning: Interest vs Rating, and fewer statuses

## Context
Two different numbers share the star vocabulary. **Interest** (1–5, `books.interest`) is how much the reader wants to read a book and drives ranking. **Rating** (0.25–5, `reading_log.rating`) is how much they liked a finished book. The queue row prints "Interest 4/5" beside "estimated 4.3★" (`templates/index.html`), the book form asks "How much does it call to you?", and `stars` renders ratings as ★. There are also six statuses (`readingStates` in `model.go`: to-read, reading, read, abandoned, archived, unreviewed) shown as raw text via `label`, where `abandoned` is displayed as "Set aside" only on one tab (`handlers_list.go`) and `archived` and `unreviewed` overlap in the reader's mind. `templates/book.html` also still says "later Goodreads checks leave them alone" although the feed checks were removed.

## Goal
Interest and ratings never look alike, and the reader sees four statuses.

## Do
1. **Rename Interest to "Want-to-read" in all user-visible text** (templates, notices, the README, the AI payload labels are internal and may stay). Show it as five pips (`●●●○○`, with `sr-only` "Want-to-read 3 of 5") using a new template func, never as stars. Reserve `★` for ratings the reader gave and for estimates (`star`, `stars`).
2. **One display vocabulary for status**, defined once (a `statusLabels` map or func used by every template, replacing the ad hoc `label` use): Queue (`to-read`), Reading, Read, Set aside (`abandoned` and `archived` display the same; keep both stored values, because imports, restore, and the ranking rules distinguish them), and To review (`unreviewed`, the library-only holding state). Check `handlers_list.go` tab labels, the status `<select>` in `book.html`, badges in `index.html`, notices, and the import preview text for every spot that prints a status.
3. **Fix stale copy:** "Goodreads checks" becomes "imports" in `book.html`, `reading.html`, `history.html`, and anywhere else `grep -ri "check\|sync\|poll" internal/app/web` finds feed-era wording.
4. Do not change stored values, JSON export keys, the restore format, or any ranking behavior. This is a display and vocabulary change only.

## Tests
- Every stored status renders a label from the single map, and no template prints a raw status key (a test that renders each page with one book of each status and asserts none of `to-read`, `unreviewed`, `abandoned` appears as visible text).
- Pips render the right count and accessible text for 1 to 5; stars still render for ratings.
- Update `a11y_test.go` and `web_test.go` expectations for the changed strings.

## Out of scope
Merging `abandoned` and `archived` in the database, changing how Goodreads shelves map to statuses.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
