# 38 · Import reconciliation and corrections

## Goal
Explain what an import will do and let the reader repair library records and manage identities remembered by deletion/merge. Keep matching deterministic and imports transactional, repeatable, and faithful to local edits.

## Scope and sequence
Implement three focused parts in order, using the existing plan flow:

1. **Explain imports.** Keep the current summary, then offer paginated record details: new/matched/skipped, matched local book, actual match key (ISBN, normalized identity, original-title fallback, Goodreads ID, or alias), proposed field/status/history changes, and local fields preserved. A no-op should say so. Do not change matching precedence or add fuzzy auto-matching. Clearly distinguish ambiguous keys from an accepted unique match.
2. **Correct library events.** Add authenticated edit and void actions from library history/book details, each previewing the event change and resulting current loan state. Preserve the original imported observation and record the reader's override or void separately so replaying the same CSV does not undo a correction or recreate a voided event. Recompute affected loan state using the existing newest-event and tie-break rules. A newer distinct imported observation may supersede corrected older state; explain this behavior. Never change reading completion, ratings, or queue order.
3. **Manage aliases.** List remembered deleted identities and merge redirects, with target links and the keys actually stored (do not assume aliases retain a display title). Allow removing a specific alias after a preview explaining that future imports may recreate the old entry or match differently. Removal does not resurrect a book immediately. Do not add bulk clear, automatic merge, or arbitrary alias retargeting in this version.

## Implementation guidance
Inspect `records.go` (`lookupBook`, `importRecords`, event reconciliation), `handlers_imports.go`, `plans.go`, `handlers_plans.go`, `merge.go`, history handlers/templates, and restore validation. Derive diagnostics from the same matching/apply logic, rather than duplicating it or changing results while adding explanations. Build preview and apply from one decision path; revision-check changes and recompute/refuse stale previews.

Keep import detail output bounded and avoid storing raw CSV uploads, discarded review text, or full personal import data in logs. Persist only correction/provenance data needed for repeat-import behavior, with explicit migration/export/restore support. Corrections must retain identity/fingerprint links across merge and respect deletion. If a new plan kind is added, give it an explicit apply branch; never let it fall through to queue reordering. Explain whether library-state corrections invalidate ranking previews because availability/deadlines changed.

## Acceptance criteria
- Prove diagnostics match actual apply results for each key, ambiguous matches, aliases/tombstones, unchanged imports, preserved local values, reading-history updates, and status advances.
- Test a corrected/voided imported event followed by identical and out-of-order re-imports; latest-event corrections, corrected source/format/date, fingerprint collisions, and newer distinct events. Correcting/voiding never creates a finished reading.
- Test alias removal preview/apply, stale previews, missing targets, subsequent re-import behavior, merge/delete of corrected books, migration, export/restore, and transaction rollback on failure.
- Repeated imports add no duplicate events/readings/corrections. Preserve existing import semantics and test fixtures except where this prompt explicitly adds a reader correction.

## Repository contract

Read `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, and `SECURITY.md` first, then inspect the current implementation. Older prompts and documentation may describe superseded behavior; use the code and tests to establish the baseline. Implement this feature, not just a design proposal.

- Preserve the single-reader Go/SQLite app, embedded server-rendered templates, plain CSS, no-JavaScript baseline, CSP, CSRF, and authenticated access. No frontend build step or mandatory external service.
- Never silently reorder the queue. Keep reading intent, reading history, progress, and library events separate; a loan or return never means finished. Preserve pins, transactional/idempotent imports, and aliases.
- Use parameterized SQL and bounded inputs. Respect the single database connection: close rows before querying again, keep transactions short, and do no network work inside `Store.Write`.
- For persisted schema changes, append the next available migration in `migrations.go`; never edit `schema.sql` or shipped migrations. Review `restore.go`, raise `maxRestoreSchema` only after compatibility work, and cover older exports, round trips, delete/merge, and existing data. Document the automatic pre-migration backup and rollback to the previous release. Do not hard-code a migration number from this prompt.
- Explicitly decide how each new mutation interacts with revision checks, stored plans, and Undo. Follow existing concurrency conventions without accidentally destroying unrelated Undo or allowing stale changes.
- Add focused regression tests with synthetic data. Run the required checks in `CONTRIBUTING.md`, including race tests, vet, formatting, and staticcheck. Check desktop and narrow mobile layouts for UI changes and capture screenshots with synthetic data. Update README behavior where needed. Report changes, validation, and remaining limitations.
