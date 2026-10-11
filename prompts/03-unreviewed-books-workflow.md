# 03 · Surface and resolve unreviewed books

## Context
Library imports (Libby, NYPL) create books with `reading_status='unreviewed'` so a loan is never mistaken for reading intent. That design is correct and must stay. The surrounding workflow is incomplete.

## Problems found (verified at `4bafb11`)
1. **Hidden.** `(*App).index` accepts `view` only in `queue | all | reading | read` (an inline `in(view, …)` list), and `index.html` has a matching tab list and heading map. Unreviewed, abandoned, and archived books appear only under "All books". There is no inbox for books awaiting a decision, and the sidebar in `base.html` does not mention them.
2. **Order-dependent import.** In `importRecords` (`store.go`), a match on an existing book only back-fills `goodreads_id`, `isbn`, and `pages`. If a book first arrives through a Libby import (`unreviewed`) and later appears in a Goodreads CSV (or RSS sync) as `to-read` or `read`, it stays `unreviewed` forever. The README says to import Goodreads first, but nothing enforces or detects the reverse order, and the dropped signal is silent.
3. **Slow triage.** Resolving an unreviewed book means opening each edit page and changing a dropdown. With hundreds of library-only titles this is impractical.

## Task
1. Add views `unreviewed` and `archived` (and `abandoned` if useful) to the tabs, and a sidebar entry with a count badge for unreviewed. Keep the allowed-view list in **one** place (a package-level slice or map used by `index` and the templates) rather than duplicated. Update the heading map in `index.html`.
2. **Upgrade-on-first-signal in `importRecords`.** When a record with `Source == "goodreads"` matches an existing book whose status is `unreviewed`, adopt the Goodreads status: `to-read` (set `needs_placement=1`; give it a new position at the end of the queue, `MAX(position)+1`, consistent with how `editBook` appends), `read`, `reading`, or `archived`. Limit this strictly to `unreviewed` books. Never override `to-read`, `reading`, `read`, `abandoned`, or `archived`. Because `importRecords` is shared with RSS sync, `newBook`, `loan`, and the demo, make the rule depend on the record's source and status explicitly so none of those paths change behavior accidentally (a `loan` passes a `Book` built from the stored row; make sure it can never trigger an upgrade). Return a count of upgrades alongside `added`/`events` (adjust the signature or return a small struct, and update all callers), and surface it in the plan preview ("N library-only books will move to your queue / finished list"), the import notice, and the `last_sync` message for RSS.
3. **Quick triage** in the unreviewed view: per-row buttons "Want to read", "Already read", and "Not interested" (→ `archived`). Each is a POST handled in `routes.go` with CSRF (middleware) and the `current()` revision check, applies the same side effects as `editBook` on a status change (clear pin, set `needs_placement`, append to queue), rejects books that are not `unreviewed`, and bumps the revision. Add a bulk "Mark all visible as…" control that goes through a **confirmation step** (a plan or confirm page showing the count and titles), not an instant change. Reuse the existing undo pattern only if it fits cleanly; the current `undo_queue` stores queue order only, so say so in the report if you do not extend it.
4. Update the README Imports section (the sentence "Existing books keep local reading status…" needs the exception) and the project description of views.

## Acceptance criteria
- Tests (package `app`, using `browser` and `addRecords`):
  - Libby import then Goodreads import moves the book out of `unreviewed` with the right status, flags, and position; same for RSS-originated records.
  - A Goodreads import never changes an existing `to-read`/`reading`/`read`/`abandoned`/`archived` book.
  - Re-running the same Goodreads import is idempotent (no second move, no extra events, revision not bumped).
  - `loan` on an unreviewed book keeps it `unreviewed` (existing `test` for return-does-not-mean-read must still hold).
  - Triage endpoints change only status/flags/position, require CSRF and a current revision, and reject non-unreviewed books.
  - The unreviewed view renders with and without books, and the sidebar badge count is right.
- Existing tests pass.

## Notes
Library events must never change reading status. Only Goodreads (explicit reader intent) or the reader may.
