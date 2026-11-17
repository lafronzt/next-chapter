# 42 · Push shelves and ratings to Hardcover

Needs 39, 40 (and 41 for the conflict model). This is the outbound half of the two-way sync.

## Goal
Let the reader send local changes to Hardcover: add a book to Want to Read, mark Currently Reading, mark Read with a rating. Always reader-initiated and previewed. The app's own queue remains the source of truth for *order*; Hardcover has no queue order, so only status, rating, and presence are pushed.

## Hardcover facts to use
- Mutations return `{ error, user_book { … } }` rather than throwing, so always check `error`. Verified: `update_user_book(id:, object:{status_id: 3})` ("Finish Reading"); the server starts or finishes a read when status changes (2 starts one, a re-read gets a new read; 3 finishes it) and writes the journal entries itself. **Do not write journal entries or reads yourself for those transitions or they appear twice.**
- Adding a book to the library is an insert on `user_books` (`insert_user_book`, scope `write:library`). Its exact argument shape is **not** confirmed by this prompt's research: introspect it (the 39 connection test) and read `docs.hardcover.app/api/graphql/actions/` before writing it, then record the shape in `testdata/hardcover/`.
- `rating` is 0–5 with halves. The app uses quarter stars: round to the nearest half on push, show the rounding in the preview ("4.25 will be sent as 4.5"), and never write the rounded value back locally.
- Status ids: 1 Want, 2 Reading, 3 Read, 4 Paused, 5 DNF, 6 Ignored. Privacy: `privacy_setting_id` 1 public, 2 followers, 3 private. **New library rows created by this app default to private (3)** unless the reader chooses otherwise in Preferences, because pushing a public shelf entry is a disclosure.
- Writes need `write:library`. The 60/min, burst-10 limits still apply; batch at most 10 top-level mutation fields per request.

## Design
1. A "Send to Hardcover" action appears on a linked book page and as a bulk action on the library (select books → preview → confirm). It never runs by itself, never after an import, and never as a side effect of reordering, marking finished, or rating. Changing a book locally can *offer* "Also update Hardcover?" on the confirmation, off by default, remembered per Preferences only if the reader opts in.
2. Preview lists, per book: what Hardcover currently has (fetched fresh just before the preview), what will change, and rounding or privacy notes. Anything where Hardcover is *ahead* (e.g. Hardcover says Read, local says to-read) is shown as a conflict and is not sent. A conflict is resolved by the reader choosing a side per row; "keep local" and "take Hardcover" (a 41 pull for that one book) are the only options. No silent merging.
3. Unlinked books can be sent only through 40's link step first. Never insert a book into Hardcover's catalog (`insert_book` and other `write:catalog` mutations are out of scope; the app never asks for that scope).
4. Local record of what was sent: table `hardcover_sync (book_id, field, local_value, remote_value, direction, at)` (migration; export/restore support; capped history) so a later pull can tell "Hardcover changed" from "we changed it" and avoid echo loops. A push followed by a pull produces no changes.
5. Never push reviews, private notes, reasons, queue position, pins, interest scores, AI estimates, or loan data. Library events (loans) and `reading_log` are separate streams and are untouched by this prompt.
6. Failures are partial and honest: a batch that fails midway reports which books were sent, which were not, and can be retried; sent ones are not resent. Mutations are not blindly retried on timeout; re-read the remote state first to see whether the write landed.

## Acceptance criteria
- Fake-server tests: add to Want to Read, advance to Reading, finish with rating, rating rounding, privacy default, `error` field populated in a 200 response, scope missing (403), 429 mid-batch, timeout then verify-before-retry, remote ahead → conflict not sent.
- Push never changes local status, ranking, pins, or order; push→pull is a no-op; no journal or read writes for status transitions (assert the request bodies).
- Request bodies contain only ids, status, rating, and privacy fields (assert no notes, review, or reasons strings).
- Migration, export/restore round trip, stale-revision rejection, CSRF on every POST.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
