# 36 · Series dashboard

## Goal
Make it easy to see progress through series already represented in the local library and choose the next known unread volume.

## Scope and behavior
- Add a Series page grouping local books by the same case-insensitive series identity used by ranking. Show volumes read, currently reading, queued, and set aside, with links to each book and its displayed status.
- Order known numeric volumes numerically, including fractional numbers. Show unknown numbers separately. Count a book once even when its reading history contains rereads. Distinguish current status from evidence of a previous completed reading.
- Identify the next known unread numbered volume without skipping an earlier known volume that is set aside or unreviewed; explain its status and offer the existing book action rather than silently changing it.
- Describe numeric gaps as “No local entry between #1 and #3,” never as proof that a required volume exists or that the series is incomplete. Unknown numbers and duplicate volume numbers must remain visible. Do not fabricate titles or treat fractional entries as mandatory prerequisites.
- Provide a series-name search and useful empty states. No external catalog fetch, automatic additions, new series ordering rules, or queue changes in this version.

## Implementation guidance
Inspect `model.go`, `propose.go` (`foldKey`, `prerequisite`, `seriesPrerequisites`), `history.go`, `listing.go`, and series tests. Share canonical grouping logic rather than adding inconsistent case rules. Avoid per-book database queries; aggregate local data with stable ordering. Paginate series/results where needed and retain filters in links. Reuse stored series fields; avoid a new series entity unless the implementation demonstrates a concrete need.

## Acceptance criteria
- Test mixed-case and Unicode names using ranking's equivalence rules, fractional numbers, missing numbers, duplicate volume numbers, blank series names, standalone books, rereads, and conflicting status/history.
- Test next-known-volume selection and gap language without making claims about unlisted books.
- Verify deterministic results, bounded queries, authenticated routes, and mobile/desktop layouts. Existing ranking output, pins, queue positions, and series constraints remain unchanged.

## Repository contract

Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first, then inspect the current implementation. Older prompts and documentation may describe superseded behavior; use the code and tests to establish the baseline. Implement this feature, not just a design proposal.

- Preserve the single-reader Go/SQLite app, embedded server-rendered templates, plain CSS, no-JavaScript baseline, CSP, CSRF, and authenticated access. No frontend build step or mandatory external service.
- Never silently reorder the queue. Keep reading intent, reading history, progress, and library events separate; a loan or return never means finished. Preserve pins, transactional/idempotent imports, and aliases.
- Use parameterized SQL and bounded inputs. Respect the single database connection: close rows before querying again, keep transactions short, and do no network work inside `Store.Write`.
- For persisted schema changes, append the next available migration in `migrations.go`; never edit `schema.sql` or shipped migrations. Review `restore.go`, raise `maxRestoreSchema` only after compatibility work, and cover older exports, round trips, delete/merge, and existing data. Document the automatic pre-migration backup and rollback to the previous release. Do not hard-code a migration number from this prompt.
- Explicitly decide how each new mutation interacts with revision checks, stored plans, and Undo. Follow existing concurrency conventions without accidentally destroying unrelated Undo or allowing stale changes.
- Add focused regression tests with synthetic data. Run the required checks in `CONTRIBUTING.md`, including race tests, vet, formatting, and staticcheck. Check desktop and narrow mobile layouts for UI changes and capture screenshots with synthetic data. Update README behavior where needed. Report changes, validation, and remaining limitations.
