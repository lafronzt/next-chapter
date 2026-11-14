# 28 · Visual direction: type, covers, and fewer eyebrows

## Context
The palette in `app.css` is good (paper `#fbfaf6`, forest `#233d35`/`#2e5749`, terracotta `#b35a3d`). Things to improve: body text is 14px (`body{font:14px/1.6 ...}`); the serif (`--serif: Georgia`) is used only for headings; every page opens with an all-caps eyebrow ("FOLLOW YOUR CURIOSITY", "A QUEUE THAT KNOWS YOUR PRIORITIES") that repeats decoration without information; cover tiles are `cover-{{cover .ID}}` with only five variants (`id % 5`), so a list of books looks repetitive and similar titles look identical.

## Do
1. **Type scale.** Body 16px (line-height ~1.6); dense tables and metadata 14px. Define a small set of CSS custom properties for sizes and use them rather than scattered `px` values. Titles and authors in the queue and book view use the serif.
2. **Covers.** Generate a distinct, deterministic tile from the book (hash of title+author, not `id`), choosing from a larger palette (e.g. 12 hues drawn from the existing colors, all with AA contrast for the tile text) plus one of a few simple patterns, all in CSS (gradients or an inline `data:` SVG that the CSP allows via `img-src data:`). Keep a fixed aspect ratio (2:3) at every size, and make the title text clamp to 4 lines with an ellipsis. Do not fetch any image from the network.
3. **Eyebrows.** Remove them from every page except the home queue and the import page; keep the page `<h1>` and one line of helpful (not poetic) subtitle where it explains the page.
4. **Spacing and density.** Use one spacing scale; reduce card padding on narrow screens; make sure no page needs horizontal scrolling at 360px.
5. **Contrast and focus.** Re-verify AA contrast for text, badges, and the focus ring with the new sizes (report measured ratios); keep the existing reduced-motion rules.
6. **No new fonts.** System and Georgia stacks only (the no-external-assets rule in `README.md`).

## Tests
- Cover selection is deterministic and stable for the same title+author, differs across a sample of 200 distinct books at least 10 ways, and never fails for empty or non-Latin titles.
- `a11y_test.go` still passes; update golden/text assertions for removed eyebrows.

## Out of scope
A new logo, animations, external cover art, a theme toggle (system dark mode is prompt 23).

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
