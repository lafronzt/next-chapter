# 09 · Accessibility and mobile pass

## Context
Server-rendered Go `html/template` pages in `internal/app/web/templates/`, plain CSS (`internal/app/web/static/app.css`, ~1,100 lines, breakpoints at 1100px and 760px), no JavaScript, strict CSP (`script-src 'self'`, `style-src 'self'`). CONTRIBUTING asks that UI changes be checked on desktop and narrow mobile layouts. Templates and assets are embedded with `go:embed`, so changes need a rebuild to preview (`LOCAL_DEMO=1 START_POLLER=0 go run ./cmd/next-chapter`).

## Observed issues to verify and fix (all confirmed present at `4bafb11` unless noted)
1. Sidebar glyphs (`≡ ▤ ◷ ↓ ⚙`) in `base.html` are plain `<span>`s that screen readers announce. Mark them `aria-hidden="true"`; do the same for decorative glyphs elsewhere (`◆ Pinned`, `↗`, `⌕`).
2. The current nav link and the selected tab use CSS classes only (`class="active"`, `class="selected"`). Add `aria-current="page"` to the active nav link and the selected tab. Also note the nav "Reading queue" highlight depends on `.View`, which is empty on non-index pages: check that exactly one item is current on every page.
3. Notices: the success notice in `base.html` has `role="status"`, but the error variants (`class="notice error"` in `login.html`, `plan.html`, `imports.html`) and the notice in `book.html` and `plan.html` have no role. Errors should use `role="alert"`; non-urgent notices `role="status"`. The generic `error.html` page should present its message in a way that is announced.
4. The Search button is labelled only by `⌕` with `aria-label="Search"`. Confirm it shows a visible focus state (there is a shared `:focus-visible` rule at the top of the stylesheet; verify it is not overridden for these controls).
5. Row actions: "Pin", "Unpin", "Keep here", and "Find its place" are ambiguous out of context. Give them accessible names including the title (for example `aria-label="Pin {{.Title}}"`), consistent with the existing "Move {{.Title}} up".
6. `disabled` buttons convey nothing about why. Add `title` and/or visually hidden text where useful (a pinned book cannot be moved; first/last position).
7. Every page has the same `<title>Next Chapter</title>` (set in the shared `top` template). Give each page a distinct title ("Reading queue – Next Chapter", "Import books – Next Chapter", …), via a `Title` value in the page data.
8. Colour contrast of muted text, badges (`.badge.warm`, `.green`, `.new`), eyebrow labels, and the topline needs checking against WCAG AA in the stylesheet (both text and non-text boundaries). Compute ratios from the CSS values rather than eyeballing.
9. Form fields: confirm each input has a programmatic label (wrapping `<label>` is fine), the import error list is a real `<ul>` with a heading, and error text is associated with the page (a summary linked to the file input is enough).
10. Keyboard-only use: tab order through the queue, focus visible on all buttons and cover links, no focus traps, a skip-to-content link before the sidebar, and `prefers-reduced-motion` handling (there is no `prefers-reduced-motion` rule in the stylesheet today).
11. Verify 320px, 375px, and 768px: no horizontal scrolling, tap targets of at least 44×44px for row action buttons, and a usable sidebar (it should collapse into a top bar; verify what the 760px rule currently does).
12. `<html lang="en">` is present; check that `viewport` allows zoom (it does today; do not add `user-scalable=no`).

## Task
Audit against the list, fix everything found, and avoid adding JavaScript. If you find additional issues, fix the low-risk ones and list the rest. Use screenshots at the three widths plus 1280px (Chrome DevTools, Playwright if available, or axe/Lighthouse). If you cannot run a browser, say so and verify what you can from the CSS and markup.

## Acceptance criteria
- A short audit table in the report: issue, location, fix, how verified.
- Tests in `internal/app` (extend the web-flow tests): `aria-current="page"` on exactly one nav item per page; `role="alert"` on error notices; `aria-hidden="true"` on nav glyphs; distinct `<title>` per page; the skip link exists and targets an existing `id`.
- No visual regressions at 1280px (describe before/after screenshots).
- CSP unchanged and still valid: no inline styles, scripts, or `style=""` attributes introduced.
- `go test -race ./...`, `go vet ./...`, and `gofmt -l .` clean.

## Notes
Do this after prompt 08 if you want templates that are readable (08 reformats the one-line templates), or accept noisier diffs.
