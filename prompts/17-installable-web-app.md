# 17 · Make it installable on a phone

## Context
Next Chapter is used mostly from one phone and one desktop. Today it opens in a browser tab with browser chrome. There is no `manifest.webmanifest`, no touch icon, and no `theme-color` (`templates/base.html` has only the viewport meta and an SVG favicon). The goal is "Add to Home Screen" gives a standalone, app-like window. This is a PWA-lite: **no service worker and no offline mode**. Offline writes would fight the revision check, and the app is self-hosted on the reader's own network anyway.

## Do
1. Add `static/manifest.webmanifest` (`name` "Next Chapter", `short_name` "Next Chapter", `start_url` `/`, `scope` `/`, `display` `standalone`, `background_color` and `theme_color` matching the CSS palette, icons). Serve it with `Content-Type: application/manifest+json`; check how `/static/` is served (`http.FileServer`) and add the type if needed.
2. Add icons as static files: 192px and 512px PNG, a maskable 512px variant, and a 180px `apple-touch-icon`. Generate them from `static/favicon.svg` (the "nc" mark); commit the PNGs. No build step, so no icon pipeline in CI.
3. In `base.html` (`top` template) add `<link rel="manifest">`, `<link rel="apple-touch-icon">`, `<meta name="theme-color">` (with a dark-mode variant via `media`), `<meta name="apple-mobile-web-app-capable">` and a status-bar style meta, and `viewport-fit=cover`. Pad the page with `env(safe-area-inset-*)` so a notch or home indicator never covers content or the fixed bottom navigation from prompt 19.
4. The login page and every authenticated page must still work when launched from the home-screen icon: sessions are cookies, so confirm a standalone launch keeps the session and that `Sign out` still works.
5. Confirm the manifest and icons are fetched without a session: `/static/` must stay public, and the manifest must not leak anything.

## Tests
- A web test that `GET /static/manifest.webmanifest` returns 200, `application/manifest+json`, valid JSON with the required members, and that every icon it lists exists in the embedded assets.
- A test that `GET /login` (unauthenticated) includes the manifest link and theme-color meta.

## Out of scope
Service worker, offline caching, push notifications.

## Ground rules
Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and `prompts/README.md` (ground rules and Go traps) first. Facts that shape UI work here:

- Templates are server-rendered Go `html/template` in `internal/app/web/templates/`; styles are one plain stylesheet, `internal/app/web/static/app.css`. No frontend build step, no framework, no CDN, no web fonts.
- The Content-Security-Policy is `default-src 'self'; style-src 'self'; script-src 'self'; ...` (`middleware.go`). **No inline `<script>`, `<style>`, or `style=""` attributes.** There is currently no JavaScript at all. Prefer HTML and CSS (`<details>`, `:target`, `:focus-within`, `:has()`); if script is truly needed, add one small `internal/app/web/static/app.js` as progressive enhancement, so every flow still works with scripts off.
- Every state change is a full-page POST with a CSRF token and a library revision (`{{template "fields" .}}`). Do not add a client-side data layer or change that contract.
- Preserve the accessibility work already in place (skip link, `aria-current`, `sr-only` text, labelled controls, reduced-motion and coarse-pointer rules) and `a11y_test.go`. New controls need accessible names and visible focus.
- Verify: `gofmt -l .` prints nothing, `go vet ./...`, `go test -race ./...`. Check the page at 390px, 768px and 1280px wide, light and dark, and report how you checked (a screenshot per width is ideal).
- Honor the product invariants: never silently reorder a queue, a loan is never reading completion, imports are idempotent. A UI shortcut must still go through the same handler and preview/undo paths.
- Keep the diff focused. Update the README only where documented behavior changed. Finish by reporting what changed, test results, and anything you chose not to do.
