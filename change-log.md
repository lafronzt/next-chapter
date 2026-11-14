# Change log

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
