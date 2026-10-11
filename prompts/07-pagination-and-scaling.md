# 07 · Pagination and large-library scaling

## Context
The README promises up to 10,000 rows per import. A long-time Goodreads user can easily have several thousand books across shelves. All requests share one SQLite connection (`SetMaxOpenConns(1)`), so any expensive request delays every other one.

## Problems found (verified at `4bafb11`)
- `(*App).index` calls `loadBooks` (all books plus all loans), filters in Go, and renders **every** matching book with one `<article>` and several forms each. It also passes `AllBooks` and the whole `Queue` to the template. At a few thousand books the page is multiple megabytes of HTML.
- `bookByID` calls `loadBooks` and scans for one ID. It is used by `editBook` (GET and POST) and `loan`; `bookAction` and `makeSuggestion` also load everything.
- `history` is capped at 500 events (`loadEvents(…, 500)`) with no paging; `editBook` GET also caps a book's events at 500.
- Import previews render only the first 100 rows (fine), but `savePlan` stores the entire parsed `Plan{Records}` as JSON in the `plans` table inside a write transaction. Each `Record` serializes the whole `Book` struct (including empty `loans`, `identity`, `position`, …), so a 10,000-row import writes a multi-megabyte row. The same payload is re-read and re-parsed on every preview GET.
- `reorder` issues one `UPDATE` per queue entry; moving one book one place rewrites the entire queue and `Poll`/other requests wait behind it.

## Task
1. Add `page` and `per_page` (default 50, max 200) to the book list with Previous/Next links that preserve `view` and `q`, and "Showing 51–100 of N". Count and filter in SQL (`WHERE reading_status = ? AND (title||author||tags) LIKE ?` with correct escaping of `%`, `_`, and your escape character; match the current case-insensitive substring semantics), ordered `ORDER BY position, id`. Load loans only for the books on the page (`WHERE book_id IN (…)`).
2. Compute the stats row (queue size, pending placement, active loans) with aggregate queries, not by loading all books. The row's "position" numbers must stay **absolute** within the whole queue: compute them with a window function (`ROW_NUMBER() OVER (ORDER BY position, id)` over `reading_status='to-read'`) or an offset count. The up/down disabled state at page boundaries must still reflect the whole queue (first/last of the entire queue, and pinned neighbors across the page edge: look up the adjacent queue row rather than assuming it is on the page).
3. Add `getBook(q queryer, id int64)` to `store.go` that loads one book and its loans, and use it in `editBook`, `loan`, and anywhere else that looks up a single book. Keep `bookByID` as a thin wrapper or remove it.
4. Paginate `/history` the same way and let it filter by source and status. Page through all events, not just 500. For a single book's events on the edit page, either paginate or keep a higher explicit cap and say "showing latest N of M".
5. Make `reorder` write only the rows whose position changed (compare against current positions in one query) using a prepared statement inside the existing transaction.
6. Imports: store the plan compactly. Options: a trimmed record struct for the plan payload and/or `compress/gzip` or `compress/flate` (stdlib) on the stored bytes. Measure a synthetic 10,000-row import before and after (stored bytes, time to save, time to GET the preview) and put the numbers in the report. Do not change the one-hour expiry or the stale-revision protection. If you change the stored format, old stored plans in the table must either still decode or be rejected cleanly as expired (they live at most an hour; prompt 05 clears `plans` on migrations that need it).
7. Remember `SetMaxOpenConns(1)`: never keep a `*sql.Rows` open on `store.DB` while running another query on it.

## Acceptance criteria
- Tests: 500 synthetic books paginate correctly; a search term applies across pages; position numbers stay absolute on page 2+; up/down disabled state is correct at the first and last queue positions and next to a pinned book across a page boundary; a page past the end returns the last page (or a clear empty state: choose and document); `/history` pages through more than 500 events; filters by source and status work; special characters in the search (`%`, `_`, quotes) match literally.
- Report response time and HTML size for `GET /` with 5,000 books before and after (use `httptest` and a temp DB) and for `GET /plans/{id}` on a 10,000-row import.
- A 10,000-row import still works end to end and 10,001 is still rejected.
- Existing tests pass.
