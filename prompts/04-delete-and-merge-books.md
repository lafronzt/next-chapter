# 04 · Delete and merge books

## Context
Matching (`findBook` in `store.go`) is conservative: Goodreads ID, then an ISBN that identifies exactly one local book, then normalized title+author, each only when exactly one row matches. The README acknowledges that subtitle or spelling variants create duplicates and lists an "explicit duplicate-merge workflow" under Next steps. There is no way to delete a book at all.

## Problems found (verified at `4bafb11`)
- No delete route in `Handler()`. A mis-typed or wrongly imported book can only be archived, so it stays in "All books" and in the JSON export forever.
- No merge. Two entries for the same book, one from Goodreads and one from Libby, split their events and loans.
- `events.book_id` and `loans.book_id` reference `books(id)` with no `ON DELETE` rule, and `PRAGMA foreign_keys=ON` is set, so a naive `DELETE FROM books` fails.
- Queue positions are **global** (`MAX(position)+1` over all books at insert), while `reorder()` renumbers only `to-read` books `0..n-1`. "Compacting" after a delete must follow the same rule: renumber the remaining `to-read` books in order using `reorder(tx, ids)`; do not touch other books' positions.

## Task
1. **Delete.** `POST /books/{id}/delete` reached from the edit page through a separate confirmation page (title, event count, "this also removes N library events and M current loans"). CSRF via middleware and the `current()` revision check. In one `Write`: delete the book's `events` and `loans`, delete the book, renumber the queue, `bump`. The revision bump already invalidates undo and previews.
2. **Merge.** A "Merge into another book…" flow on the edit page: search by title/author (reuse the list search), choose the target, then a preview page that spells out the result. The target keeps its title, author, status, interest, pin, and position. Fill blank target fields (`goodreads_id`, `isbn`, `pages`, `series`, `series_number`, `tags`) from the source. Move events, de-duplicating: `events.fingerprint` is `sha256(source|identity(title,author)|format|status|at|due_date)`, so it embeds the **source book's identity**. Recompute each moved event's fingerprint for the target's identity (build a `Record` with the target's title/author and call `fingerprint`), and skip an event if the target already has one with the same `(source, format, status, at, due_date)` or the new fingerprint. Rebuild `loans` for the target from the merged events with the existing newest-wins rule (`observed_at` greater, or same time/status with a changed due date). Delete the source. Respect the unique indexes on `identity` and `goodreads_id`: if both books have different non-empty Goodreads IDs, refuse with a clear message, and if both have different non-empty ISBNs say so in the preview (allowed, keep the target's).
3. **Tombstones so re-import does not resurrect.** Add `book_aliases(identity TEXT PRIMARY KEY, goodreads_id TEXT NOT NULL DEFAULT '', isbn TEXT NOT NULL DEFAULT '', target_book_id INTEGER NULL)`. Insert rows on merge (identity of the source, pointing at the target) and on delete (target `NULL`, meaning ignore). Teach `findBook` to consult aliases (by Goodreads ID, ISBN, and identity, with the same exactly-one rule) after the live-table lookup, returning the alias target. `importRecords` must skip tombstoned records (no insert, no events) and the count must appear in the import preview and notice. Include the table in `Store.Backup()` (it is part of the file, so it just needs to not be scrubbed) and in the JSON export. When a deleted book is later re-added **manually**, remove the matching tombstone so the reader's explicit action wins.
4. Update README ("Imports and data ownership", "Next steps") and CONTRIBUTING if invariants change.

## Requirements
- Schema change: use an explicit migration (prompt 05). If 05 has not been done, add the table with `CREATE TABLE IF NOT EXISTS` in `schema.sql` and note the dependency in your report.
- Never merge or delete automatically. Everything is reader-initiated and previewed.
- Keep `findBook`'s signature usable by `newBook`, `editBook`, and `loan`.

## Acceptance criteria
- Tests: delete removes events and loans and leaves queue order intact with contiguous `to-read` positions; delete is blocked on a stale revision; merge moves events without duplicates and preserves the target's status, pin, and position; merge refuses conflicting Goodreads IDs; the moved events' fingerprints match the target identity, so re-importing the same Libby CSV after a merge adds nothing; a re-import after delete or merge does not resurrect the book and is idempotent; alias rows appear in a `Backup()` copy and in the export.
- Existing tests pass.
