# 35 · Borrowing calendar

## Goal
Show upcoming borrowing commitments together: loan due dates, ready-for-pickup deadlines, and the reader's estimated hold arrivals. Help the reader notice overlapping loans without moving books or contacting a library.

## Scope and behavior
- Add a Library calendar page with a date-range navigation and an accessible chronological agenda that works on mobile without JavaScript. Start with the next 30 days, with an explicit overdue section. Distinguish firm deadlines from estimated hold arrivals using words as well as styling.
- Derive entries from current loans, not every historical event. Show title, source, format, event type, date, book link, and existing remaining-progress/pace information where applicable. Preserve separate loans for different formats and sources.
- Summarize overlapping active dated loans factually. Do not invent checkout start dates or promise feasibility without reading-pace data. Do not infer returns from missing records.
- Add an authenticated downloadable `.ics` snapshot for the selected bounded range. This is a manual calendar import, not a public subscription or live sync; explain that later changes are not pushed to a calendar app. No email, push notifications, scraping, or public access tokens.

## Implementation guidance
Inspect `progress.go`, `handlers_home.go`, `records.go`, `history.html`, routing, and export handlers. Reuse existing date and progress helpers. This should normally require no schema migration.

Use RFC 5545 all-day events with deterministic UIDs based on local book/source/format/type, escaped text, CRLF lines, UTF-8-safe line folding, UTC DTSTAMP, and exclusive next-day DTEND. Mark expected hold arrivals clearly in both UI and calendar summaries. Bound the date range and event count. Do not embed credentials, private notes, ratings, or sensitive internal URLs. Fetch the data consistently, then format outside the database transaction.

## Acceptance criteria
- Test month/year boundaries, leap days, overdue and missing dates, source/format separation, changed deadlines, and returned/cancelled loans disappearing from current commitments.
- Test calendar content type/download headers, authentication, stable UIDs, text escaping, line folding with multibyte characters, and all-day end dates.
- Calendar views and downloads make no outbound requests and leave queue order, reading status, library state, and Undo untouched.

## Repository contract

Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first, then inspect the current implementation. Older prompts and documentation may describe superseded behavior; use the code and tests to establish the baseline. Implement this feature, not just a design proposal.

- Preserve the single-reader Go/SQLite app, embedded server-rendered templates, plain CSS, no-JavaScript baseline, CSP, CSRF, and authenticated access. No frontend build step or mandatory external service.
- Never silently reorder the queue. Keep reading intent, reading history, progress, and library events separate; a loan or return never means finished. Preserve pins, transactional/idempotent imports, and aliases.
- Use parameterized SQL and bounded inputs. Respect the single database connection: close rows before querying again, keep transactions short, and do no network work inside `Store.Write`.
- For persisted schema changes, append the next available migration in `migrations.go`; never edit `schema.sql` or shipped migrations. Review `restore.go`, raise `maxRestoreSchema` only after compatibility work, and cover older exports, round trips, delete/merge, and existing data. Document the automatic pre-migration backup and rollback to the previous release. Do not hard-code a migration number from this prompt.
- Explicitly decide how each new mutation interacts with revision checks, stored plans, and Undo. Follow existing concurrency conventions without accidentally destroying unrelated Undo or allowing stale changes.
- Add focused regression tests with synthetic data. Run the required checks in `CONTRIBUTING.md`, including race tests, vet, formatting, and staticcheck. Check desktop and narrow mobile layouts for UI changes and capture screenshots with synthetic data. Update README behavior where needed. Report changes, validation, and remaining limitations.
