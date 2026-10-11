# 05 · Schema migration framework

Do this first. The Go rebuild made it more urgent.

## Context
`OpenStore` (`internal/app/store.go`) runs the embedded `schema.sql` on **every start** (`CREATE … IF NOT EXISTS`, `INSERT OR IGNORE` for `schema_version = 1`) and then runs a second, ad-hoc upgrade keyed on a *different* meta key, `go_data_version`: for any database without it, it rewrites every `events.at` and `loans.observed_at` into a new canonical nanosecond format, **deletes all `plans` and `undo_queue` rows**, and sets `go_data_version = 1`. That is how Python-era (v0.1) databases are brought into the Go app. Nothing ever reads `schema_version`.

CONTRIBUTING.md says: "Schema changes must include an explicit migration and backup/rollback instructions; do not modify a deployed schema implicitly." Right now:
- The one-way timestamp rewrite happens **without any backup**, on first start of a new image over a user's real database. After it, the old Python image can no longer interpret the data safely, so there is no rollback.
- A downgrade (older Go image against a newer database) is not detected.
- Several planned changes (prompts 02, 03, 04, 12) need schema changes.

## Task
1. Add a small ordered migration runner (new file `internal/app/migrate.go`):
   - `type migration struct{ version int; name string; apply func(*sql.Tx) error }`, and a package-level list. Version 1 is the Python-era baseline, whose statements must remain identical to today's so existing databases are untouched. Version 2 is the existing `go_data_version` rewrite (timestamp canonicalization plus clearing `plans`/`undo_queue`), moved into a normal migration. Treat a database that already has `go_data_version >= 1` as being at version 2 and remove the old key, in the same transaction that records the new version. Make migration 2 safe on an empty database.
   - Create a brand-new database by running migrations from zero, so there is one code path. `schema.sql` should become the v1 baseline (without the `INSERT OR IGNORE` seed lines the runner now owns).
   - On startup, take the write lock **before** reading the version. `Store.Write` uses a deferred transaction; use `BEGIN IMMEDIATE` for the migration (for example through the driver's `_txlock=immediate` DSN option if the pinned modernc version supports it, otherwise execute `BEGIN IMMEDIATE` on a dedicated `*sql.Conn`). Verify whichever you choose with a test. Apply each pending migration in its own transaction, updating `schema_version` in the same transaction, so a failure rolls back that step completely.
   - **Before applying any pending migration to an existing, non-empty database**, write a consistent backup beside the database with `VACUUM INTO` (already used by `Store.Backup()`), named like `next-chapter.sqlite3.pre-v2.bak`, mode 0600, and log it. Skip for a new database. Do not overwrite an existing backup of the same name; add a numeric suffix. Fail startup if the backup cannot be written.
   - Refuse to start with a clear error if the database's `schema_version` is **newer** than the code knows, so an old image never writes to a newer schema.
2. Read `schema_version` for `export()` (it is hardcoded as `1` in `routes.go`) and show the schema version on the Preferences page.
3. Do **not** invent production schema changes here. Test the machinery with an injected test-only migration list (make the list a parameter of an internal `migrate(db, list, path)` function).
4. Documentation: README "Backup and restore" and CONTRIBUTING get (a) how to add a migration, (b) the automatic pre-migration `.bak`, (c) rollback: stop the app, restore the `.bak` to `next-chapter.sqlite3` (and delete stray `-wal`/`-shm`), run the previous image. Also state plainly that upgrading a Python-era database to the Go app applies a one-way timestamp rewrite, a `.bak` is created first, and the Python image should not be run against the migrated file. The existing `.bak` lives on the data volume, so mention disk space.

## Acceptance criteria
- Tests with temp databases:
  - A fresh DB ends at the latest version with the expected tables and no `go_data_version` key.
  - A Python-era fixture (reuse the setup in `TestLegacyMigration`) upgrades with data preserved, timestamps canonicalized, `plans`/`undo_queue` cleared, and a `.bak` created that opens as the pre-upgrade state.
  - A DB with `go_data_version = 1` and no new version is recognized as v2 with no data rewrite and **no** backup needed unless a later migration is pending.
  - An injected v3 upgrades a v2 DB with data preserved and a `.bak`; a failing injected migration rolls back fully, leaves the version unchanged, and keeps the `.bak`.
  - A DB at a higher version than the code refuses to start.
  - Starting twice is a no-op the second time (no extra backup, no writes).
  - Two `OpenStore` calls racing on the same file path both succeed and migrate once.
- Existing tests pass. `TestLegacyMigration` may be adapted but must keep asserting the same data guarantees.
