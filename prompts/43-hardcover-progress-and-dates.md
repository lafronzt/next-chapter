# 43 · Two-way reading progress and read dates

Needs 39, 40, 41, 42. Builds on 37 (progress checkpoints) and 29 (reading history).

## Goal
Keep the *current read* in step between the apps: progress while reading, start and finish dates, and re-reads. This is the only part of the integration where both sides change often, so the rules for who wins must be explicit and tested.

## Hardcover facts to use
- A library entry (`user_books`) has many reads (`user_book_reads`): `started_at`, `finished_at`, `finished_at_precision` (1 day, 2 month, 3 year), `progress_pages`, `progress_seconds` (audiobooks), `edition{pages audio_seconds}`.
- Verified mutations (scope `write:library`): `update_user_book_read(id:, object:{progress_pages:, action_at:})`, `insert_user_book_read(user_book_id:, user_book_read:{started_at, finished_at, finished_at_precision})`, `update_user_book_read(id:, object:{started_at:…})` (only sent fields change; `null` clears a date), `delete_user_book_read`.
- "Find the read in progress" query: the read with `finished_at: {_is_null: true}`, `order_by:{id:desc}, limit:1`.
- Server behavior: setting status 2 starts a read (a new one on a re-read), 3 finishes it, progress that reaches the end of the edition finishes the read and marks the book Read, and starting, progress and finishing each create a journal entry. **Do not create journal entries yourself** (`insert_reading_journal` is out of scope here). Use `action_at` to date a progress update that happened earlier.
- Progress is pages (or seconds); there is no percent field. Compute percent from the edition's `pages` or `audio_seconds`.

## Design
1. **Streams stay separate.** Progress and checkpoints (37) and `reading_log` (29) never change status, pins, or order, and a Hardcover progress update never changes the local queue. A pulled finish may *offer* the existing quick Finished flow (20/27) but does not apply it on its own.
2. **Direction per field, by default:**
   - Progress: local → Hardcover when the reader logs a checkpoint and has opted in ("Also update Hardcover progress", per book, default off). Hardcover → local only through an explicit "Pull progress" on the book page.
   - Read dates: pushed only as part of 42's finish or a "Send this read" action; pulled through 41. A local `source='manual'` row is never overwritten by a pull, and a Hardcover read is never deleted or edited by a pull of local data.
   - Re-reads: map one Hardcover read to one local reading-history entry via a stored `hardcover_read_id` (migration; export/restore) so an edited date updates the same entry instead of adding a duplicate.
3. **Conflicts**: if both sides changed since the last sync (use 42's `hardcover_sync` table), show both values and let the reader pick; default is no change. Never average, never pick the later automatically.
4. **Precision**: keep month-only and year-only finish dates as such on both sides; the history page already sorts by finish date, so decide and document how a year-only date sorts (end of period is wrong; use a separate "date unknown within year" bucket).
5. **Rate use**: progress pushes are single mutations (1 request). Never poll; the reader's action triggers one read-in-progress query then one mutation.
6. Edge cases to define and test: progress beyond the edition's page count; local pages differ from Hardcover edition pages (convert via percent and say so in the preview); audiobook seconds vs pages; finishing locally when Hardcover has no open read; Hardcover finishing a book that is still `to-read` locally.

## Acceptance criteria
- Fake-server tests for each mutation above, with request-body assertions (no journal writes, `action_at` used for backdated entries), the finish-at-end-of-edition server behavior, re-read creates a second read and a second local entry, precision 1/2/3 round trips.
- A pulled or pushed progress change leaves status, pins, queue order, loans, and ranking output unchanged; manual `reading_log` rows untouched.
- Idempotence: push then pull, and pull then pull, add no duplicate entries; conflict preview shown when both sides changed.
- Migration, export/restore, stale revision, CSRF; `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
