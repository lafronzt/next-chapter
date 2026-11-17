# 44 · Hardcover as a book-details source, and complete series

Needs 39 and 40; extends 32 (lookup) and 36 (series dashboard).

## Goal
Use Hardcover's catalog to fill missing details and a description (as another `BookSource` beside Open Library and Google Books), and to show the *whole* series including volumes the reader does not own, so the series dashboard can say "next unread volume" with real data.

## Hardcover facts to use
- Catalog reads need `read:catalog` and carry no user data. `books` has title, subtitle, description, pages, release year, rating and counts, `contributions{author{name}}`, `image{url}`, `book_series{position series{name}}`; `editions` has ISBNs, pages, `audio_seconds`, publisher; `series` / `book_series` give the ordered members of a series (see the "Getting All Books in a Series" guide).
- **Position can be fractional or null** (novellas, omnibuses). Treat `series_number` as REAL (it already is) and keep null.
- Terms: Hardcover data is community-curated and used "at your own risk". Aggregates (a Hardcover average rating) may be shown if labeled as Hardcover data. User-uploaded images require a DMCA policy if the page is public; this app is private, but **still do not copy images to disk**.
- The ranking and the AI estimate must not depend on Hardcover community data by default.

## Design
1. **Fill, never overwrite** (same rules as 32): propose before/after per field, only empty fields pre-selected, a typed description is never replaced, markup stripped, text capped, rendered escaped, and any text sent to a model sits inside `<library_data>` with the untrusted notice. Source is shown ("from Hardcover").
2. Add `hardcover` to the lookup source order after the user's linked id is known (an exact id lookup is better than a text search); fall back to ISBN, then title+author via `search`. Reuse the merge step from 32 to prefer the value most sources agree on.
3. **Series completion:** for a linked book with a series, one query returns the ordered members. The series dashboard (36) lists unowned members as "not in your library" with a one-click "Add to my list" that creates a normal local book (needs-placement, `source='hardcover-series'`) after a preview. Nothing is added automatically and the queue never reorders. Cache the series result locally with a fetched-at time so the dashboard renders offline and costs no requests; refresh only on reader action.
4. **Covers:** show a cover only by hotlinking Hardcover's image URL in an `<img>` with `referrerpolicy="no-referrer"`, `loading="lazy"`, an `alt`, and behind a Preferences switch (off by default). That requires widening the CSP `img-src` for exactly that host; do it narrowly and say so in the PR. The existing hashed cover tiles (28) remain the default and the failure fallback. Never download or store the image, and do not add an image proxy.
5. **Community rating (optional display):** a muted "Hardcover average 4.1 (12,431)" on the book page when linked and enabled. Not used by `estimate.go`, `reasons.go`, or ranking. If the reader later wants it as an estimate input, that is a separate decision.
6. Migration for any new columns (e.g. `books.hardcover_rating`, a `series_cache` table) with export/restore handling and rollback notes; all cached remote data is droppable and rebuilt on demand, so restore may omit it.

## Acceptance criteria
- Fake-server tests: id lookup, ISBN fallback, search fallback, null/fractional series positions, series with gaps, a book in two series, 404/429/timeout, oversized body, markup in descriptions stripped.
- Fill-only-empty semantics proven; Hardcover data never reaches `buildEstimatePayload` unless the book already stored it as its own description; ranking output unchanged by a lookup or series refresh.
- Cover switch off means zero image requests and unchanged CSP; on means exactly one allowed host; a11y test still passes (alt text).
- Series dashboard shows unowned volumes without creating books; adding one is previewed and lands as needs-placement.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
