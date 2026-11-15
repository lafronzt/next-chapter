# 37 · Reading sessions and pace

## Goal
Optionally retain dated progress checkpoints so the reader can see recent pace and an approximate finish date. The current page or percentage remains quick to update; tracking must not become a requirement for reading.

## Scope and behavior
- Add an explicit, off-by-default option to retain progress history. When off, the existing progress form continues saving only the latest value. When enabled, a successful progress save also records a dated checkpoint for that reading attempt. Label these as progress updates, not measured time spent reading.
- Show recent checkpoints on the book page, with correction and deletion actions. Keep page and percentage units distinct. Allow backward corrections and progress resets without interpreting them as negative reading speed.
- Separate rereads into distinct attempts. Starting a new attempt does not reuse the old attempt's pace. Define how existing in-progress books acquire an attempt without inventing a historical start date or retroactively generating checkpoints.
- Calculate pace only from comparable checkpoints in the current attempt and a documented recent window. Require at least two dated observations on different days with positive elapsed time and progress. Use elapsed calendar days, describe the estimate plainly, and return “Not enough recent progress” for sparse, stale, mixed-unit, reset, or unsuitable data. Never extrapolate from zero pace.
- Show an approximate finish date and, where meaningful, compare it with an existing loan deadline. No streaks, reminders, inferred completion, timers, or automatic queue changes.

## Implementation guidance
Start with `progress.go`, status/start/finish handlers, `history.go`, `book_view.html`, and progress tests. Inject the clock for deterministic calculation. Define same-day checkpoint behavior, timezone/date conventions, unit changes, total-page edits, resets, and correction of the latest checkpoint. Never silently let the displayed current value disagree with history; state whether an edit also updates current progress.

Persist attempts/checkpoints with valid book references, bounded values, and indexes. Provide a way to clear retained history; disabling retention stops new entries and explains whether existing entries remain. Include history/settings in export/restore, preserve attempts on merge, and remove them on book deletion. Ensure queue Undo cannot discard later progress data or attach checkpoints to the wrong attempt.

## Acceptance criteria
- Test retention on/off, same-day updates, resets/backward corrections, unit switches, rereads, sparse/stale data, zero pace, page-count changes, and date boundaries.
- Test the pace/finish arithmetic using fixed dates, mutation concurrency, migration, export/restore, merge/delete, and progress saved after a queue action followed by Undo.
- Reaching 100% or the last page never marks a book finished; only the existing explicit finish workflow does.

## Repository contract

Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first, then inspect the current implementation. Older prompts and documentation may describe superseded behavior; use the code and tests to establish the baseline. Implement this feature, not just a design proposal.

- Preserve the single-reader Go/SQLite app, embedded server-rendered templates, plain CSS, no-JavaScript baseline, CSP, CSRF, and authenticated access. No frontend build step or mandatory external service.
- Never silently reorder the queue. Keep reading intent, reading history, progress, and library events separate; a loan or return never means finished. Preserve pins, transactional/idempotent imports, and aliases.
- Use parameterized SQL and bounded inputs. Respect the single database connection: close rows before querying again, keep transactions short, and do no network work inside `Store.Write`.
- For persisted schema changes, append the next available migration in `migrations.go`; never edit `schema.sql` or shipped migrations. Review `restore.go`, raise `maxRestoreSchema` only after compatibility work, and cover older exports, round trips, delete/merge, and existing data. Document the automatic pre-migration backup and rollback to the previous release. Do not hard-code a migration number from this prompt.
- Explicitly decide how each new mutation interacts with revision checks, stored plans, and Undo. Follow existing concurrency conventions without accidentally destroying unrelated Undo or allowing stale changes.
- Add focused regression tests with synthetic data. Run the required checks in `CONTRIBUTING.md`, including race tests, vet, formatting, and staticcheck. Check desktop and narrow mobile layouts for UI changes and capture screenshots with synthetic data. Update README behavior where needed. Report changes, validation, and remaining limitations.
