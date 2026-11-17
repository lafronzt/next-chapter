# 41 · Import your library from Hardcover (pull)

Needs 39 and 40. Same contract as the Goodreads and Libby imports: transactional, idempotent, previewed, never reorders the queue.

## Goal
Bring the reader's Hardcover library into Next Chapter: new books as "needs placement", status and rating changes, and finished dates, using the same preview → plan → apply path as the CSV imports. Hardcover becomes a third import source, and a faster replacement for the Goodreads CSV round trip.

## Hardcover facts to use
- `user_books(where:{user_id:{_eq:$me}}, order_by:{id:asc}, limit:, offset:)`; no row cap on `user_books`, but cap pages yourself (e.g. 100 rows) so a request stays under the 30 s timeout. Nested selections are free, so fetch `book{id title slug pages contributions{author{name}} series info}`, `edition{id isbn_13 isbn_10 pages}`, and `user_book_reads{id started_at finished_at progress_pages}` in the one query. `user_book_reads` caps at 1,000 rows per request.
- Status ids: 1 Want to Read, 2 Currently Reading, 3 Read, 4 Paused, 5 Did Not Finish, 6 Ignored.
- `rating` is numeric 0–5 and supports halves. `private_notes` and `review` exist; **do not import reviews or notes** (see Task).
- Use `updated_at` to make later pulls incremental: `where:{updated_at:{_gt:$since}}`. Store the high-water mark in `meta`. A full re-pull must always be possible and must produce the same result.
- Reading user_books needs `read:library`. Each top-level field is one request, so one page is one request.

## Design
1. **Status map (documented in the README and one table in code):** 1→`to-read` (new books "needs placement"), 2→`reading`, 3→`read`, 4→`reading` flagged paused only if the app has a paused concept, otherwise leave the local status and report it as skipped with a reason, 5→ the existing did-not-finish handling (inspect `model.go`; if none, skip with a reason, do not invent one), 6→ skip entirely and remember it as ignored in the preview. Never move a book backward (read → to-read); show it as "Hardcover disagrees" and leave local alone.
2. **Matching order:** `hardcover_book_id` (from 40) → ISBN → normalized title+author → aliases. Unlinked matches are linked as part of the same plan (shown in the preview). Books Hardcover has and the app does not become new rows, `source='hardcover'`, appended as "needs placement". Aliases (deleted/merged identities) must still be honored.
3. **Ratings:** Hardcover halves map exactly into the app's quarter-star scale. A reader-entered or Goodreads-imported rating is **not overwritten** unless the reader ticks "use Hardcover's rating" per row in the preview; the default keeps local. Record provenance so a repeat pull is a no-op.
4. **Dates:** write `reading_log` rows with `source='hardcover'`. A `source='manual'` row is never overwritten. `finished_at_precision` (1 day, 2 month, 3 year) must be respected; do not invent a day for a month-only date.
5. **Out of scope on purpose:** reviews, private notes, journal entries, lists, goals, likes, other users. Reviews and notes stay in their own streams (33) and are not copied from Hardcover without a separate reader decision.
6. **Preview first.** Summary counts (new, matched, status advances, rating changes, disagreements, skipped, unmatched), a paginated detail list like 38's reconciliation view that shows the match key and local fields preserved, then Apply. A no-op pull says so.
7. Idempotent: a second identical pull adds no books, events, readings, or ratings, and changes no order. Imports append "needs placement" and never touch positions or pins.

## Task
`hardcover_import.go` producing `importRecords`-compatible records (reuse the shared path; remember it is shared by CSV import, `newBook`, `loan`, and the demo), `handlers_hardcover_import.go`, template extending the import page from 22 with a "Hardcover" tab (disabled with an explanation when not configured), `plan()` branch for the new kind (a new plan kind must get its own branch), README.

## Acceptance criteria
- Fixture-driven tests (synthetic): every status id, half-star ratings, month-only and year-only finish dates, re-reads (multiple reads), Hardcover book merged to a new id, ignored books, books with no ISBN, paging across more than one page, incremental `since` pull equals full pull, mid-pull `429` resumes without partial apply.
- Local-wins tests: manual reading_log row, local rating, pinned book, queue order all unchanged after apply; ranking output byte-for-byte identical for already-placed books.
- Transaction rollback on a failure partway through apply; stale-revision rejection; alias honored; second pull is a no-op.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
