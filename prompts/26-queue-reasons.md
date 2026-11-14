# 26 · Say why each book is where it is

## Context
Ranking reasons exist (`Suggestion.Reasons` in `model.go`, produced by `propose.go`) but appear only inside suggestion previews. The queue itself is a list of positions with no explanation, so the reader can't tell a ranked book from one they placed by hand, or why a library book is near the top.

## Do
1. Compute **one short reason per queued book** and show it as a quiet line under the title: examples "Library loan due in 5 days", "Pinned", "You moved this", "Next in the Foo series", "Short, fits your mood", "Needs a spot". Choose the single most informative reason by a fixed precedence (hand-placed and pinned first, then library deadline, then series order, then mood or length fit, then nothing). **No reason is better than a vague one.**
2. Reuse the scoring functions in `propose.go`; do not duplicate the rules in the template. The queue must not get slower: `loadQueue` and the list handler already run a few queries per page, so compute reasons from data already loaded for the page (or one extra query for the page's IDs) and add no per-row queries. Remember the single DB connection (`prompts/README.md`).
3. Visually distinguish **hand-placed** books (`moved_by_hand` or `pinned`) from ranked ones: an icon plus text, not color alone, and a legend or `title` explaining it once on the page.
4. The reason line is plain text and screen-reader friendly. Hide it on very narrow screens only if prompt 18's row would overflow; otherwise keep it, since it is the point of this change.
5. The queue's **order is unchanged by this work**. Reasons explain; they never move anything.

## Tests
- Reason precedence for each case above, in table-driven tests against `propose.go`'s helpers.
- A page with 200 queued books issues no per-row queries (assert query count with a counting wrapper or by reusing the existing scale test in `scale_test.go`).
- Positions and `needs_placement` are identical before and after rendering the page.

## Out of scope
New ranking factors, showing numeric scores.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
