# Contributing

Open an issue describing the reading workflow you want to improve, or submit a focused pull request. Run `python -m unittest discover -s tests -v` before submitting. For UI changes, check both desktop and narrow mobile layouts.

Keep the project self-hostable without mandatory paid services. Preserve the separation between reading intent, queue order, and library events. Imports must be idempotent, transactional, and must not interpret a loan/return as completion. Never silently reorder someone's queue. Add regression coverage when changing ranking constraints, identity matching, or import behavior.

Please use synthetic or anonymized CSV fixtures. Do not commit real library cards, passwords, tokens, personal exports, database backups, or `.env` files. Explain the source and limitations of new integrations; avoid promising unavailable history or unsupported two-way sync.

Schema changes must include an explicit migration and backup/rollback instructions; do not modify a deployed schema implicitly. The initial schema is version 1. SQLite currently supports a single app replica.
