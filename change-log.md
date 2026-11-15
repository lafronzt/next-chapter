# Change log

## Prompt 38 (import reconciliation and corrections)

| Part | Change |
|---|---|
| 1 Explain imports | `matchBook` (shared by `lookupBook`) reports the matching key, any alias, and refused ambiguous keys. `importRecordsExplained` is the one import function; `importRecords` calls it without a collector, and the details page calls it with one inside a rolled-back savepoint, so explanations cannot differ from apply. `GET /plans/{id}/details` (`import_details.go`) pages and filters decisions, refuses a stale preview, and stores nothing. |
| 2 Correct events | Migration 16 adds `event_corrections` (keyed by event fingerprint). `corrections.go`: edit, void, or restore through a `correction` plan with its own apply branch in `applyPlan`; `rebuildLoans` now reads effective events. `importRecords` leaves loans alone for an event that has a correction, so replays neither undo an edit nor recreate a void. Merge re-keys or carries corrections with events, delete removes them, export/restore carry them (`maxRestoreSchema` 16). |
| 3 Aliases | `aliases.go`: `GET /aliases` lists stored keys and targets; removing one goes through an `alias-remove` plan with its own apply branch that refuses a record that changed. No bulk clear or retargeting. |
| Tests | `reconcile_test.go`. Backup before migration 16: `<database>.pre-v16.bak`. |

## Prompt 37 (reading sessions and pace)

| Prompt | Change |
|---|---|
| 37 | Migration 15 adds `reading_attempts` and `progress_checkpoints`; `Preferences.ProgressHistory` (off by default, Privacy section) turns retention on. `progress_history.go`: `noteStatusChange` (called from `setStatus`, the edit form, and Undo's `restoreStates`) opens and ends attempts, `recordCheckpoint` (called by `bookProgress` in the same transaction) stores one update per attempt per day, `estimatePace` works from the newest unbroken same-unit run within the window, and `editCheckpoint` / `clearProgressHistory` correct and clear. Book page shows updates, pace, an approximate finish date, and a due-date comparison. Merge moves attempts and updates, delete removes them, export/restore carry them (`maxRestoreSchema` 15). Progress and updates never bump the revision. Tests in `progress_history_test.go`. Backup before the migration: `<database>.pre-v15.bak`. |

## Prompt 35 (borrowing calendar)

| Prompt | Change |
|---|---|
| 35 | `calendar.go` and `calendar.html` (`GET /calendar`, `GET /calendar.ics`): one query reads current loans (borrowed, ready, hold); a pure `buildCalendar(loans, today, start, days)` sorts them into overdue, an agenda for a bounded range (default 30, at most 92 days, at most 500 entries), and undated loans; firm deadlines and hold estimates are labeled in words; overlap is described without inventing checkout dates. `buildICS` writes RFC 5545 all-day events with deterministic UIDs, UTC DTSTAMP, escaped text, CRLF, and UTF-8-safe folding. Clock is `a.now`. No schema change, no outbound requests, no queue or Undo effect. Tests in `calendar_test.go`. |

## Prompt 36 (series dashboard)

| Prompt | Change |
|---|---|
| 36 | `series.go` and `series.html` (`GET /series`): one query loads every book with a series, grouped by `foldKey` (ranking's rule), ordered deterministically. Numbered volumes sort numerically, unnumbered and duplicate numbers stay visible, rereads count a book once, finished history is shown apart from status. The next known whole-numbered unread volume is not skipped past set-aside or To-review books; fractions never block; gaps use "No local entry between" wording. Series link in the sidebar and More panel. No schema change, no queue effect. Tests in `series_test.go`. |

## Prompt 34 (saved library views)

| Prompt | Change |
|---|---|
| 34 | Migration 14 adds `books.owned` and `saved_views`. `bookFilter` gains exact tag, series, page range / unknown pages, one-loan source/format/status (EXISTS), and ownership; `parseListFilter` is the single reader for URLs, forms, and saved definitions, and `values()` their canonical form. SQL functions `nc_fold` and `nc_hastag` give exact, Unicode-aware matching without LIKE. `views.go` holds the versioned definition, CRUD handlers (`/views`, `/views/{id}`), and validation; they never bump the revision or touch Undo. Pager links keep every filter. Ownership is on the add/edit forms, kept by imports, OR-ed on merge. Export/restore carry `saved_views` and `owned` (`maxRestoreSchema` 14). Tests in `views_test.go`. Backup before the migration: `<database>.pre-v14.bak`. |

## Prompt 33 (private reading notes)

| Prompt | Change |
|---|---|
| 33 | Migration 13 adds `books.reason` and the `book_notes` table (versioned rows, page or percent, bounded text). `notes.go` holds validation and the add/edit/delete handlers (`POST /books/{id}/notes`, `POST /notes/{nid}`); they leave the revision, Undo, queue, status, and progress alone. Reason field on the add and edit forms; notes section on the book page. Delete removes notes; merge moves them and keeps a conflicting reason as a note, shown in the preview. Export/restore carry `notes` and `reason` (`maxRestoreSchema` 13). Tests in `notes_test.go` prove notes never reach AI requests, previews, or lookups. Backup before the migration: `<database>.pre-v13.bak`; rollback is to restore it and run the previous release. |

## Reading workflow (progress, holds, yearly goal)

| Change | Detail |
|---|---|
| Progress | Migration 11 adds `books.progress_page` and migration 12 adds `books.progress_percent` (a book has at most one; percent is for audiobooks). `POST /books/{id}/progress` (`progress` plus `unit` of `page` or `percent`) saves it for a book being read; Reading now and the book page show it, and Due soon shows what is left (pages or percent) and the amount a day to finish in time. It never bumps the revision, so previews and Undo stay valid. Finishing clears it. |
| Holds | Migration 11 adds `loans.expected_ready`, the reader's own estimate, entered on the library form for the activity "hold". Within 14 days it adds up to 10 points (scaled by library timing) with a reason, and lists on the home screen under Holds arriving. Leaving the hold clears it; restore and merge keep it. |
| Yearly goal | `Preferences.ReadingGoal` (no schema change), set on the Reading history page. Counts readings finished this year and says ahead, on, or behind an even pace. |
| Fixes | Radio buttons in the lookup comparison no longer inherit text-input sizing, and its field headings no longer sit on the divider line. A re-import now strips "(Series, #N)" from the title of an already stored, unedited book. |
| Add-a-book lookup | `POST /books/new/lookup` and a Look up details button on the Add a book form (when book lookup is on) fill the form's empty fields from the databases, for the reader to check before adding; it saves nothing. The form gains an optional ISBN, and a looked-up description keeps its source unless the reader rewrites it. |
| Tests | `progress_test.go`, `series_title_test.go`. Backups before migrations 11 and 12: `<database>.pre-v11.bak` and `<database>.pre-v12.bak`; rollback is to restore the older one and run the previous release. |

## Prompt 32 (book details lookup)

| Prompt | Change |
|---|---|
| 32 | `lookup.go` (Open Library, Google Books, optional ISBNdb behind a `BookSource` interface), `handlers_lookup.go`, migration 10 (`books.description`, `description_source`), a Book lookup switch in Preferences, a Details block on the book page, an opt-in lookup step on import previews, and the description in the estimate candidate. Tests in `lookup_test.go`. |

## Prompts 17–21 (UI pass)

| Prompt | Change |
|---|---|
| 19 | Bottom tab bar (Queue, To review, Add, Reading, More) at ≤760px; sidebar link list, topline and footer hidden there. Tests in `a11y_test.go`. |
| 18 | Queue rows keep ↑/↓ and a "⋯" `<details>` sheet holding Find its place, Pin, Keep here, Move to #, Predict my rating, Edit book. No handler changes. |
| 21 | One `rating-input` template (radio groups for 0.25–5 plus Unrated) used by the add and edit reading forms and the Finished form; `stars` shows a partial star for fractions. |
| 20 | `loadHome`, `POST /books/{id}/status` (start/stop), `GET/POST /books/{id}/finish`; Next up, Reading now, Due soon on page 1 of the queue. |
| 17 | Web manifest, 192/512/maskable/apple-touch icons, theme-color, `viewport-fit=cover`, safe-area padding. No service worker. |

## Prompts 22–25 (stacked on the 17–21 branch)

| Prompt | Change |
|---|---|
| 24 | One status vocabulary (`statusLabels`), want-to-read pips instead of "Interest n/5", stale "Goodreads checks" copy removed. Stored values unchanged. |
| 22 | Goodreads-first import page with a numbered guide and drop zone; Libby/NYPL collapsed; "last imported" in `meta`; preview leads with a summary row and names the books leaving the queue. |
| 23 | Dark palette, All books status filter and sort (`sort`, `status` params), `#book-ID` redirects, collapsed book form, sticky search, `app.js` submit-disable. |
| 25 | `GET/POST /place` placement sessions; plan application extracted to `applyPlan` and shared with `/plans/{id}`. |

## Prompts 26–31 (stacked on the 22–25 branch)

| Prompt | Change |
|---|---|
| 27 | `/books/{id}` is a read-only page with one primary action per status; the form moves to `/books/{id}/edit`. |
| 26 | `queueReason` (reasons.go): fixed precedence, two queries per page, hand-placed rows marked with an icon and legend. |
| 28 | 16px type scale and spacing scale as CSS variables, 12 hashed cover colors x 4 patterns at 2:3, eyebrows removed except Home and Import. |
| 29 | Reading history: year sections, rating distribution table, year / rating / unrated filters that page. |
| 30 | Signed `n=` notices naming the book, single-slot Undo for moves, pins, Keep here and status changes, empty states with a next step. |
| 31 | Preferences in Ranking / Privacy / AI sections that save independently; confirmation before deleting review text; About footer. |
