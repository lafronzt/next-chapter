# 13 · Track reading history from Goodreads

Foundation for the AI prompts (14–16): they need a trustworthy record of what the reader has finished, when, and how they rated it. Do prompts 05 and 12A first; 06 is strongly recommended.

## Context
Today Goodreads data is only used to create books and statuses. `parseRecord` (CSV) and `ParseRSS` read title, author, IDs, ISBN, pages, shelves, and exclusive shelf. They discard everything about *reading history*: when the book was added, when it was finished, the reader's rating, and their review. Library events (Libby/NYPL) are a separate stream and must stay separate.

## What Goodreads actually gives us (confirm against the examples, do not assume)
- **CSV export** (`Settings → Export library`): `My Rating`, `Average Rating`, `Date Read`, `Date Added`, `Exclusive Shelf`, `Bookshelves`, `Read Count`, `Original Publication Year`, `Publisher`, `My Review` (can be long).
- **RSS** (`/review/list_rss/<id>?shelf=…`): `user_rating`, `user_read_at`, `user_date_added`, `average_rating`, `book_published`, and the review text. Public shelves only, limited page size, and Goodreads has retired its public API, so RSS + CSV are the supported paths. Do not add scraping, password/cookie capture, or any unofficial API. Say in the README what history is *not* available (for example reading progress, re-read dates beyond `Read Count`).

## Task
1. **Schema (via migration framework):** add a `reading_log` table, one row per finished or abandoned reading: `book_id`, `finished_at` (nullable; date only), `rating` (nullable 1–5), `source` (`goodreads`/`manual`), `review_chars` (store the length only by default; see privacy below), `fingerprint UNIQUE` for idempotent re-import. Also add nullable `books.published_year` and `books.avg_rating`. Keep `events` (library) untouched.
2. **Parse:** extend CSV and RSS parsing to read the fields above with the same bounds/validation style as today (bad dates are row errors in CSV; ignored with a count in RSS). `Date Read` formats vary (`2024/03/09`, `2024-03-09`, blank); reuse `parseTime`.
3. **Import rules:** for a Goodreads record whose shelf is `read`, upsert one `reading_log` row (fingerprint of goodreads_id/identity + finished date + rating). Re-importing is a no-op. A reader's local edits to rating in the app (see 4) win over a later Goodreads value, consistent with "local choices are authoritative". Never change `reading_status`, `interest`, or queue order from history data (prompt 03 governs the unreviewed→status upgrade; 12A governs initial interest).
4. **Sync:** make the hourly poller also fetch the `read` shelf (a second shelf setting, default `read`, validated like `shelf`), through the same hardened fetch path from prompt 06. Show "last read-shelf sync" and the count of newly logged books. Because RSS is capped, document that the first full history comes from CSV and RSS keeps it current.
5. **UI:** a "Reading history" page (paginate; see 07): finished date, title, your rating, and simple stats (books per year, average rating, average pages, top tags/shelves). Allow editing the reader's own rating/finish date on the book page (manual entries have `source='manual'` and are never overwritten by sync). Plain HTML/CSS, no JavaScript, no charts library (CSS bars are fine).
6. **Export/backup:** include `reading_log` in the JSON export and in the restore flow (prompt 12B). Reviews: store only `review_chars` unless the reader opts in to keeping review text locally (a Preferences checkbox, off by default) because review text is personal data and is the largest thing the AI prompts could send out.

## Acceptance criteria
- Tests: CSV and RSS fixtures (synthetic) produce the right log rows; re-import and re-poll are idempotent and do not bump the revision; a manual rating edit survives a later sync; bad dates reject the CSV row with a row number; the history page paginates and the stats are correct on a seeded library; reading history never alters `reading_status`, `interest`, or queue positions.
- README: Imports section (new fields), what's not available, and the privacy default for reviews.
- Existing tests pass.
