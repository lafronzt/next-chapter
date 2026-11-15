# 33 · Private reading notes

## Goal
Preserve why a book caught the reader's attention and their thoughts while reading it. Notes are private local writing, distinct from book descriptions, imported Goodreads reviews, and the optional intent attached to one AI request.

## Scope and behavior
- Add an optional “Why I want to read this” field on the book edit page and a private notes section on the book view. Support multiple dated notes with create, edit, and delete actions; allow notes for every reading status.
- Keep this first version plain text. Choose and document modest character limits, preserve line breaks, render escaped text, and provide useful empty and validation states. No rich-text editor, public sharing, or automatic quote extraction.
- Optionally associate a note with a page or percentage, but do not change reading progress when writing a note. A note is not a finished reading or rating.
- Never populate, overwrite, or delete notes through CSV import or metadata lookup. Never include notes or the reason-for-adding field in AI payloads, exact-prompt previews, provider logs, or remote lookup requests. Do not add a send-to-AI option in this version.

## Implementation guidance
Inspect `model.go`, `books.go`, `handlers_books.go`, `book_view.html`, `book.html`, `history.go`, `ai.go`, `estimate.go`, and `rerank.go`. Use a dedicated notes table with stable IDs and timestamps rather than overloading `description` or `reading_log.review`. Store the reason-for-adding separately.

Add the fields and notes to JSON export/restore with bounded validation and valid book references. Older exports restore without notes. Deleting a book removes its notes; merging moves all source notes to the retained book without dropping identical text. Preserve a conflicting reason-for-adding as a note, and explain this in the merge preview. Ensure plan/Undo snapshots cannot erase notes saved after a queue action.

## Acceptance criteria
- Create, edit, and delete notes; handle empty text, oversized text, missing books, and stale edits safely.
- Malicious HTML is displayed as text. Imports and lookup preserve local writing.
- Test AI payload construction with distinctive note text and prove it is absent from all request paths.
- Test migration, export/restore, merge/delete, and queue Undo with notes added after the queue action. Note actions leave queue order, status, ratings, and progress unchanged.

## Repository contract

Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first, then inspect the current implementation. Older prompts and documentation may describe superseded behavior; use the code and tests to establish the baseline. Implement this feature, not just a design proposal.

- Preserve the single-reader Go/SQLite app, embedded server-rendered templates, plain CSS, no-JavaScript baseline, CSP, CSRF, and authenticated access. No frontend build step or mandatory external service.
- Never silently reorder the queue. Keep reading intent, reading history, progress, and library events separate; a loan or return never means finished. Preserve pins, transactional/idempotent imports, and aliases.
- Use parameterized SQL and bounded inputs. Respect the single database connection: close rows before querying again, keep transactions short, and do no network work inside `Store.Write`.
- For persisted schema changes, append the next available migration in `migrations.go`; never edit `schema.sql` or shipped migrations. Review `restore.go`, raise `maxRestoreSchema` only after compatibility work, and cover older exports, round trips, delete/merge, and existing data. Document the automatic pre-migration backup and rollback to the previous release. Do not hard-code a migration number from this prompt.
- Explicitly decide how each new mutation interacts with revision checks, stored plans, and Undo. Follow existing concurrency conventions without accidentally destroying unrelated Undo or allowing stale changes.
- Add focused regression tests with synthetic data. Run the required checks in `CONTRIBUTING.md`, including race tests, vet, formatting, and staticcheck. Check desktop and narrow mobile layouts for UI changes and capture screenshots with synthetic data. Update README behavior where needed. Report changes, validation, and remaining limitations.
