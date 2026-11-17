# 40 · Link local books to Hardcover books

Needs 39. Do before 41–46; they all use the link.

## Goal
Give every local book an optional, reader-confirmed link to its Hardcover book (and, when known, edition), so later prompts can pull and push by id instead of guessing by title. Linking is deterministic matching plus a preview. No model decides identity.

## Hardcover facts to use
- `books` and `editions` are queried with Hasura filters; `editions` has `isbn_10`, `isbn_13`, `asin`, `pages`, `audio_seconds`, `book_id`. A user's library row is `user_books` (`book_id`, `edition_id`, `status_id`, `rating`, …).
- Free-text search is `search(query:, query_type:"Book", per_page:, fields:, weights:)` (Typesense; returns a JSON `results` blob; **2 s timeout**; `_like`/`_regex` filters are disabled). Default fields are `title,isbns,series_names,author_names,alternative_titles`. Searching by ISBN hits the `isbns` field; prefer a direct `editions(where:{isbn_13:{_eq:…}})` lookup for ISBNs because it is exact.
- Merged books keep `original_book_id` on user_books. A linked id can later redirect; handle that when a pull returns a different id for the same edition.

## Design
1. Migration: `books.hardcover_book_id INTEGER`, `books.hardcover_edition_id INTEGER`, `books.hardcover_slug TEXT NOT NULL DEFAULT ''`, plus a unique partial index on `hardcover_book_id` where not null. Add to export, JSON restore (older exports without them still restore), merge (keep target's, else source's; two different links on a merge is a conflict shown in the merge preview, never silently chosen), delete (the book's alias row must keep the Hardcover id so a re-import does not relink wrongly), and plans that carry book fields.
2. Matching order, mirroring the import contract: stored `hardcover_book_id` → ISBN-13/10 via `editions` → normalized title + author via `search`. Only the first two may auto-propose with high confidence. Every proposal shows *why* it matched (the key), title, author, year, pages and cover-free metadata side by side with the local book.
3. **Never auto-link.** Entry points: a "Link to Hardcover" action on the book page (one book), and "Find Hardcover matches" on the library (batch, bounded to N books per run to respect the 10-per-burst / 60-per-minute budget; show progress and let the reader continue). Each result is `confident`, `ambiguous` (several candidates, reader picks), or `none`. The reader confirms through the existing plan flow with a revision check. A link never changes status, pins, order, ratings, or reading history.
4. Unlink is a previewed action. Relinking to a different Hardcover book is a previewed action that warns the sync history for that book will be reset.
5. What is sent: ISBN, title, author only. Never notes, reasons, ratings sent as query text, or reading history.

## Task
`hardcover_match.go` (pure candidate scoring, no I/O, unit-testable like `propose.go`), `handlers_hardcover_link.go`, templates, README note. Reuse normalization from `records.go` rather than duplicating it.

## Acceptance criteria
- Tests with a fake Hardcover server: ISBN exact hit, ISBN missing then title/author fallback, ambiguous results, no results, search timeout/429 mid-batch resumes cleanly, a different book already linked to the id (unique index conflict shown, not a 500).
- Link/unlink/relink previews and stale-revision rejection; linking never changes ranking output (assert byte-for-byte equality of the queue before and after).
- Merge and delete behavior with links; restore of old and new exports; migration test.
- Batch run never exceeds the burst limit per request and stops on `Retry-After`.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
