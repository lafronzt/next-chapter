# Contributing

Open an issue describing the reading workflow you want to improve, or submit a focused pull request. Run the checks below before submitting. For UI changes, check both desktop and narrow mobile layouts.

## Required checks

CI runs these on every pull request, and they must pass locally first. Use the tool versions pinned in `.github/workflows/ci.yml`:

```sh
gofmt -l .                       # must print nothing
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go test -race ./...              # about 10 seconds
go mod tidy && git diff --exit-code go.mod go.sum
```

CI also runs `govulncheck ./...` (it fails only on vulnerabilities the code can reach), reports test coverage without enforcing a number, validates `deploy/kubernetes/` with `kubeconform`, and builds the image and runs `scripts/smoke.sh` against it. Run the smoke test yourself with `docker build -t next-chapter:local . && scripts/smoke.sh next-chapter:local`.

The parsers have fuzz targets. CI runs only their seed corpus, as part of `go test`. After changing a parser, fuzz it for a while, for example `go test -run '^$' -fuzz '^FuzzParseCSV$' -fuzztime 60s ./internal/app`, and commit any failing input that `go test` writes under `internal/app/testdata/fuzz/` together with the fix.

## Pinned dependencies

Workflows pin every third-party action to a full commit SHA, with the release in a trailing comment. Dependabot proposes updates to both, grouped into one weekly pull request. To bump an action by hand, resolve the release tag to the commit it points at, and use the `^{}` line for annotated tags:

```sh
git ls-remote https://github.com/actions/checkout 'refs/tags/v7.0.1' 'refs/tags/v7.0.1^{}'
```

Then replace the SHA and the comment everywhere that action appears. Read the release notes before a major-version bump, and check that the inputs the workflow uses still exist.

The Dockerfile pins the `golang:1.27-alpine` build image by digest. The build stage also supplies the CA bundle copied into the final `scratch` image. Dependabot's `docker` ecosystem proposes new digests. To bump by hand, take the digest of the multi-architecture index, not of one platform:

```sh
docker buildx imagetools inspect golang:1.27-alpine   # use the top-level "Digest:"
```

Tool versions such as `staticcheck`, `govulncheck`, and `kubeconform` are pinned in the `env` block of `ci.yml`. Bump them there, and in the commands above.

Keep the project self-hostable without mandatory paid services. Preserve the separation between reading intent, queue order, and library events. Imports must be idempotent, transactional, and must not interpret a loan/return as completion. Never silently reorder someone's queue. Add regression coverage when changing ranking constraints, identity matching, or import behavior.

Please use synthetic or anonymized CSV fixtures. Do not commit real library cards, passwords, tokens, personal exports, database backups, or `.env` files. Explain the source and limitations of new integrations; avoid promising unavailable history or unsupported two-way sync.

Reading history (`reading_log`) is a separate stream like library events: it must never change reading status, interest, pins, or queue order, and a reading the reader edited or logged (`source='manual'`) is never overwritten by an import or sync. Keep review text out unless the reader opted in.

Never delete or merge books automatically; both are reader-initiated and previewed. A deleted or merged book leaves an alias in `book_aliases`, and imports must keep honoring those aliases so the book is not recreated. Event fingerprints include the book identity, so code that moves events between books must recompute them.

## Schema changes

Schema changes must include an explicit migration and backup/rollback instructions; do not modify a deployed schema implicitly. SQLite currently supports a single app replica.

`internal/app/schema.sql` is the frozen version 1 baseline, and it must not be edited. To change the schema:

1. Append `{version, name, apply}` to `migrations` in `internal/app/migrations.go`, using the next version number. Never edit or reorder a migration that has shipped.
2. Do everything inside the given `*sql.Tx`. The runner records `schema_version` in the same `BEGIN IMMEDIATE` transaction, so a failure rolls the whole step back.
3. Add tests that upgrade a database from the previous version with data intact. Use the internal `migrate(db, list, path)` with an injected list to cover failure cases.
4. Check that JSON restore (`internal/app/restore.go`) still reads exports from the new schema, then raise `maxRestoreSchema`. `TestRestoreSchemaVersionIsReviewed` fails until you do.
5. Document user-visible effects and rollback in the README. Before a pending migration runs on an existing database, the runner writes `<db>.pre-v<N>.bak`. To roll back, stop the app, restore that file over the database, delete stray `-wal`/`-shm` files, and run the previous image. An older image refuses to open a newer schema.
