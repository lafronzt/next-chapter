# 21 · A rating control that works on a phone

## Context
Goodreads ratings can be set in quarter stars and the app stores them exactly (`reading_log.rating` is a REAL from 0.25 to 5; see `parseRating`, `interestFor`, and the quarter-star migration). The manual rating field in `templates/book.html` (two places) is a `<select>` with 20 options ("4.25 of 5"), which is slow with a thumb and hard to scan. Whole-star **interest** (1–5) is a separate setting and stays whole.

## Do
1. Build a rating control with plain HTML/CSS (no JavaScript required): a row of five large star radios for the whole star, and a second row of three "+¼ / +½ / +¾" radios (or a fieldset of 20 visually grouped radios that collapses to stars), with an explicit **Unrated** option. The submitted value stays the same `rating` field with values `0.25`–`5`, so `readingFields` is unchanged.
2. The current value is pre-selected (`rated`), shown as text ("4.25 out of 5"), and announced to screen readers; the group has a `<legend>`; keyboard arrows work (native radio behavior); focus is visible.
3. Use the same control wherever a rating is entered: the book page's add-reading and edit-reading forms, and the Finished form from prompt 20. One template (`{{define "rating-input"}}`), not three copies.
4. Display: `stars` shows whole stars plus the exact value; check it at 390px and that `0.25` renders as one half-empty star plus "0.25", not as "☆☆☆☆☆".
5. Do not change Interest (1–5) input or ranking.

## Tests
- Every quarter value 0.25…5 round-trips through the form, the DB, and the export/restore path; invalid values still return 400; "Unrated" stores NULL, not 0.
- `a11y_test.go`: the control has a legend and each option an accessible name.

## Out of scope
Rating on the queue row, half-star display glyphs beyond what `stars` already does.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
