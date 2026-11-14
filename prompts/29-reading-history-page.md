# 29 · Reading history worth opening

## Context
`/reading` (`templates/reading.html`, `historyStats` in `history.go`) lists finished readings with a few totals (`readingStats`: counts, average rating, average pages, per-year counts, top tags). Ratings are now quarter stars, so a distribution is meaningful, and "books per year" with what you thought of each is the page's natural payoff.

## Do
1. **Year sections.** Group readings by finish year (undated readings in their own group at the end), newest year first, each with its count and average rating. Within a year, show each book as a compact tile (cover from prompt 28 or the existing tile) with the title, the rating as stars plus exact value, and the finish month. Re-reads are separate entries on the same book (link them to the book page).
2. **Rating distribution.** A small bar chart of how many readings fall at each rating bucket. Buckets: whole stars 1 to 5, with quarter-star detail available (a second row or tooltip text, not color alone). Plain CSS (`div`s with widths computed server-side or `<progress>`/`<meter>`), no chart library, no script, and an equivalent visible table or `sr-only` text for screen readers.
3. **Filters** that survive paging: year, rating at least, and "unrated". Query parameters, handled in `history.go`; the existing pagination keeps them.
4. **Keep the totals**, but present them as a short sentence ("47 books across 3 years, averaging 3.9★") before the chart.
5. Everything is read-only and never changes status, interest, pins, or order (`CONTRIBUTING.md`). Review text shows only if the reader opted in (`KeepReviews`); never render it otherwise.
6. Performance: stats are a few aggregate queries; keep them aggregate (no per-reading queries) and close each `*sql.Rows` before the next query on `store.DB`.

## Tests
- Year grouping, ordering, undated handling, and per-year averages with quarter-star data (4.25, 3.75).
- The distribution counts sum to the number of rated readings; unrated are excluded from the average and shown as "unrated".
- Filters combine and paginate correctly; review text never appears without the opt-in.

## Out of scope
Goals, streaks, comparisons to other readers, exporting the page.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
