# 34 · Saved library views

## Goal
Let the reader revisit useful subsets of their library without reconstructing filters. A saved view selects and sorts books; it never changes their actual queue positions.

## Scope and behavior
- Extend All books filters with exact tag, series, page-count range, and current borrowing source/status/format, alongside existing status, text search, and sort. Combine different filters with AND; explain unknown page counts and allow an explicit “pages unknown” choice.
- Add a minimal reader-managed ownership flag, default false, so “short books I own” is possible. Ownership is independent of borrowing, format, and reading status; imports never infer it and library events never clear it.
- Save the normalized current filter and sort under a bounded name. List, open, rename, update, and delete saved views. Use a stable view ID, with paginated results and a clear indication when temporary URL filters differ from the saved definition.
- Example views: “Owned, under 250 pages,” “Borrowed ebooks,” and “Queued fantasy.” Show empty results without deleting the view. No arbitrary SQL, user-written filter expressions, or separate queues.

## Implementation guidance
Start with `listing.go`, `handlers_list.go`, `index.html`, `books.go`, and the pagination/filter tests. Keep one validated filter model for URL parsing, saved definitions, counting, and listing; allowlist sort keys. Use EXISTS for matching loans so multiple loans do not duplicate books or break counts. Exact tags must not accidentally match substrings. Preserve filters in pagination and return links.

Persist saved views in a dedicated table, version their definitions if needed, and include them and ownership in export/restore. Missing fields in older exports get safe defaults. Define merge ownership as true if either book is owned. Existing imports preserve local ownership. If other prompts already added fields or filters, reuse them rather than creating duplicates.

## Acceptance criteria
- Test combined filters, exact tags, unknown pages, multiple matching loans, Unicode, literal wildcard input, invalid values, and pagination/count agreement.
- Test the full saved-view lifecycle, stale edits, stable URLs, and export/restore of definitions and ownership.
- Filtering and sorting never mutate positions, pins, placement flags, or reading status. Existing queue movement restrictions still apply to sorted/filtered views.

## Repository contract

Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first, then inspect the current implementation. Older prompts and documentation may describe superseded behavior; use the code and tests to establish the baseline. Implement this feature, not just a design proposal.

- Preserve the single-reader Go/SQLite app, embedded server-rendered templates, plain CSS, no-JavaScript baseline, CSP, CSRF, and authenticated access. No frontend build step or mandatory external service.
- Never silently reorder the queue. Keep reading intent, reading history, progress, and library events separate; a loan or return never means finished. Preserve pins, transactional/idempotent imports, and aliases.
- Use parameterized SQL and bounded inputs. Respect the single database connection: close rows before querying again, keep transactions short, and do no network work inside `Store.Write`.
- For persisted schema changes, append the next available migration in `migrations.go`; never edit `schema.sql` or shipped migrations. Review `restore.go`, raise `maxRestoreSchema` only after compatibility work, and cover older exports, round trips, delete/merge, and existing data. Document the automatic pre-migration backup and rollback to the previous release. Do not hard-code a migration number from this prompt.
- Explicitly decide how each new mutation interacts with revision checks, stored plans, and Undo. Follow existing concurrency conventions without accidentally destroying unrelated Undo or allowing stale changes.
- Add focused regression tests with synthetic data. Run the required checks in `CONTRIBUTING.md`, including race tests, vet, formatting, and staticcheck. Check desktop and narrow mobile layouts for UI changes and capture screenshots with synthetic data. Update README behavior where needed. Report changes, validation, and remaining limitations.
