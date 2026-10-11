# Contributing

Open an issue describing the reading workflow you want to improve, or submit a focused pull request. Run `go test -race ./...` and `go vet ./...` before submitting. For UI changes, check both desktop and narrow mobile layouts.

Keep the project self-hostable without mandatory paid services. Preserve the separation between reading intent, queue order, and library events. Imports must be idempotent, transactional, and must not interpret a loan/return as completion. Never silently reorder someone's queue. Add regression coverage when changing ranking constraints, identity matching, or import behavior.

Please use synthetic or anonymized CSV fixtures. Do not commit real library cards, passwords, tokens, personal exports, database backups, or `.env` files. Explain the source and limitations of new integrations; avoid promising unavailable history or unsupported two-way sync.

Never delete or merge books automatically; both are reader-initiated and previewed. A deleted or merged book leaves an alias in `book_aliases`, and imports must keep honoring those aliases so the book is not recreated. Event fingerprints include the book identity, so code that moves events between books must recompute them.

## Schema changes

Schema changes must include an explicit migration and backup/rollback instructions; do not modify a deployed schema implicitly. SQLite currently supports a single app replica.

`internal/app/schema.sql` is the frozen version 1 baseline, and it must not be edited. To change the schema:

1. Append `{version, name, apply}` to `migrations` in `internal/app/migrate.go`, using the next version number. Never edit or reorder a migration that has shipped.
2. Do everything inside the given `*sql.Tx`. The runner records `schema_version` in the same `BEGIN IMMEDIATE` transaction, so a failure rolls the whole step back.
3. Add tests that upgrade a database from the previous version with data intact. Use the internal `migrate(db, list, path)` with an injected list to cover failure cases.
4. Document user-visible effects and rollback in the README. Before a pending migration runs on an existing database, the runner writes `<db>.pre-v<N>.bak`. To roll back, stop the app, restore that file over the database, delete stray `-wal`/`-shm` files, and run the previous image. An older image refuses to open a newer schema.
