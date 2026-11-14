# 22 · An import page built for CSV re-imports

## Context
Goodreads removed its public shelf feeds in 2026 and the RSS poller was deleted. The only way new Goodreads books and status changes arrive is the reader exporting a CSV and importing it again, so this page is now the heart of the sync workflow. A re-import matches existing books (ISBN, then title+author, then Goodreads Book ID), logs readings, appends new books to the queue, and moves queued books to Reading or Read when Goodreads says so, never backwards (`importRecords` in `records.go`, `advances`). The page (`templates/imports.html`, `plan.html`) still reads like a generic importer.

## Do
1. **Lead with Goodreads.** A short numbered guide at the top: "1. On Goodreads open My Books → Import and export → Export Library. 2. Download the CSV. 3. Drop it here." Keep Libby and NYPL below, collapsed.
2. **File picker that works on a phone:** a large drop zone that is really a labelled `<input type="file" accept=".csv,text/csv">`, showing the chosen filename. No JavaScript required.
3. **Show "last imported"** on the page: when the last Goodreads import was applied and what it did (new, finished, started). Store it in the existing `meta` table if nothing records it yet (a schema change needs a migration per `AGENTS.md`; prefer `meta` JSON, which does not).
4. **Preview leads with a summary row**: counts of New books, Finished, Started, Already here, Skipped (deleted), Ratings read, Rows needing attention. The record table stays below, collapsed by default beyond the first 20 rows, and filterable to just the changes.
5. Move the "books that will leave your queue" list to the top of the preview, named, so the reader sees exactly which finished/started books will move before applying. Nothing is applied until Confirm; Confirm still uses the revision check; keep the stale-preview handling.
6. Plain-language errors with row numbers for bad rows (already produced by `ParseCSV`), with a "download the problem rows" link only if it is cheap; otherwise leave it.
7. Remove any remaining wording about feeds, polling, or "check Goodreads now".

## Tests
- Update the import web tests for the new summary and ordering; the plan JSON format must stay backward compatible (old stored plans still decode).
- Importing the bundled `examples/goodreads.csv` twice shows "0 new, 0 to change" the second time.

## Out of scope
New import sources, scheduled imports, Goodreads login.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
