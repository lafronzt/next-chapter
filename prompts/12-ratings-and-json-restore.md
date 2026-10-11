# 12 · Goodreads ratings and JSON restore

Two related data-ownership features. They can be separate commits. If you do B, do prompt 05 first.

## A. Use the Goodreads rating as the initial interest

### Problem (verified at `4bafb11`)
`interest` (1–5) drives 10–50 points of every score and defaults to 3. A Goodreads export includes `My Rating` (0–5, 0 = unrated) and `Average Rating`, but `parseRecord` in `imports.go` reads neither, and the RSS feed's `user_rating`/`average_rating` are ignored too. `importRecords` hardcodes `3` in its INSERT (`… r.Tags, 3, r.Status …`), so every imported book starts at 3 and the first "Rethink my queue" is driven mostly by tags and length. `newBook` works around this by inserting and then running `UPDATE books SET interest=?`.

### Task
- In `parseRecord`, for `source == "goodreads"`, read `My Rating`. Set `Record.Interest` from it when it is 1–5. Treat 0 or blank as unrated and leave the default of 3. Do the same for RSS `user_rating` if present. `Average Rating` must never be used (it is not the reader's opinion). Out-of-range or non-numeric values (for example 7, `abc`): ignore the value and keep 3 rather than failing the import; justify the choice in the report, and count them so the preview can mention "N ratings ignored".
- Make `importRecords` insert `r.Interest` **only for new books** and never change `interest` on an existing book (local choices are authoritative). **Trap:** several callers build records with `Interest: 0` (the demo records in `demo()`, any `Record{Book: b}` constructed without it), and the table has `CHECK(interest BETWEEN 1 AND 5)`. Clamp: use `r.Interest` only when it is 1–5, else 3. Then delete the post-insert `UPDATE` workaround in `newBook` (it passes the form's validated interest straight through).
- Show in the import preview how many new books will get a rating-derived interest, and how many existing matched books keep their local interest. That count needs the plan page to know which records are new: compute it with `findBook` at preview time, or store the count when the plan is saved (the plan is revision-checked, so it stays valid).
- Update README (Imports and data ownership): list `My Rating` among Goodreads fields and state the rule. Rated books with status `read` are stored as usual; interest matters only if they re-enter the queue.

### Acceptance
Tests: a new rated book gets that interest; an unrated (0 or blank) one gets 3; re-importing after the reader changed interest does not change it; a rating of 7 keeps 3 and is counted; the demo and manual add still work; RSS rating path if implemented; the preview shows the counts.

## B. JSON import / restore

### Problem
The README says "JSON import/restore is not implemented; use the SQLite backup." `/export.json` is described as portable readable data but cannot be loaded back, so it cannot be used to migrate to another instance or recover when only the JSON was kept. The export is also thin: it hardcodes `schema_version: 1`, carries books (with `loans`), events, and preferences, but not the Goodreads connection settings.

### Task
- Add an "Import a Next Chapter JSON export" option on the Imports page. It follows the existing safety model: validate, build a preview (counts of books, events, loans, and any conflicts), store it as a plan, apply only after confirmation, and only if the revision is unchanged.
- **Plan kind:** `plan()` currently does `if kind == "import" { … } else { /* reorder */ }`. Add an explicit `"restore"` branch (and make the default branch an error) so a restore plan can never be applied as a reorder. `Plan` currently has `Rows`, `Records`, `CandidateID`, `Source`; add a dedicated field (or a separate payload type) for the restore data rather than overloading `Records`.
- **Upload limit:** the outer handler wraps every POST body in `http.MaxBytesReader(w, r.Body, maxUpload)` (2 MB) and `ParseMultipartForm(maxUpload)` before routing. The restore route needs up to 10 MB: apply a per-route limit (decide the limit from `r.URL.Path` before wrapping) and keep 2 MB everywhere else. Also keep stored plan size reasonable (see prompt 07).
- Restore into an **empty** library only (no rows in `books`). If the library has books, refuse with a clear message; merging is a separate design.
- Preserve: book fields, queue order and positions, pins, `needs_placement`, status, interest, series, tags, `added_at`, `source`; events with their fingerprints; rebuilt loans (newest-event-wins, same rule as `importRecords`); preferences. Assign **new** book IDs on insert and remap events through the old→new ID map; do not trust incoming IDs. Do not import `plans`, `undo_queue`, login state, or `session_epoch` (prompt 02). Accept a Goodreads sync config if present but restore it with polling **disabled** and tell the reader.
- Validate strictly, reusing the same bounds as the forms and CSV (title ≤500, author ≤300, interest 1–5, pages 1–99,999, series number range, status in `readingStates`, event status in `libraryStates`, timestamps through `parseTime`, due dates `YYYY-MM-DD`), size ≤10 MB, at most 10,000 books and a sane cap on events, unique identities and Goodreads IDs, and reject unknown top-level keys (`json.Decoder.DisallowUnknownFields`) with a clear message. Recompute `identity` from title/author rather than trusting the file, and verify each event fingerprint or recompute it.
- Export: read `schema_version` from `meta` (prompt 05) and add the sync config (without secrets; there are none) so a round trip is lossless. The format version must be checked on import: unknown or newer versions are refused with the supported range in the message.
- Update the README Backup section and remove the "not implemented" sentence.

### Acceptance
Tests: export → fresh empty app → import → export produces equivalent data (compare after normalizing `exported_at` and IDs); import into a non-empty library is refused; malformed JSON, an unknown key, an unknown schema version, an out-of-range field, and an oversized body are each rejected without changing the database (assert revision and row counts); a stale revision blocks apply; queue order, pins, `needs_placement`, and loans survive the round trip; the restore route accepts a 5 MB file while another route still rejects 2 MB + 1; a restored sync config has polling disabled.

## Notes
If prompt 05 is not done, keep `schema_version: 1` as the format version and say so in the report.
