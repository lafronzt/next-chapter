# 10 · Expand test coverage

## Context
There are 8 Go tests in a single 412-line file, `internal/app/app_test.go` (`TestRanking`, `TestSeriesPinsReservePrerequisites`, `TestCSVAndRSS`, `TestImportReconciliationAndBackup`, `TestLegacyMigration`, `TestAuthenticatedWebFlow`, `TestImportPreviewAndSettings`, `TestDemoAndLoginLimit`), passing in about 1s with **72.1% statement coverage** (`go test -cover ./internal/app`). They cover ranking constraints, import integrity, the legacy-database upgrade, CSRF, stale previews, and the main web flow. Helpers in that file: `must`, `testApp`, `addRecords`, `records`, `ids`, and the `browser` type.

## Gaps found
Run `go test -coverprofile=c.out ./internal/app && go tool cover -func=c.out` first and confirm; expect at least:
- **Sync:** `FetchGoodreads` and `Poll` have no tests (see prompt 06, which adds the seams; coordinate so you do not build the same hooks twice).
- **Backup and restore:** `TestImportReconciliationAndBackup` should be extended to open the backup, check content, and verify `plans` and `login_attempts` were scrubbed and that `New` on the backup works. Note `Backup()` does **not** clear `undo_queue`; decide whether it should and test it.
- **Export:** `export.json` content, field names, and the `schema_version` value are not asserted.
- **Login:** session persistence (cookie reuse, expiry, tampered signature, oversized cookie) and the 429 path details.
- **Headers:** CSP, `X-Frame-Options`, `X-Content-Type-Options`, `Referrer-Policy`, `Cache-Control: no-store` on pages but not on `/static/`.
- **Edit flow:** status transitions in `editBook` (pin cleared, `needs_placement`, queue append), duplicate-identity conflict, `bookAction` up/down/pin/placed including the pinned-neighbor refusal, and the `current()` revision conflict.
- **Loan form:** invalid due date, invalid format, unknown source or status.
- **Dates:** `parseTime` formats (epoch seconds and milliseconds, RFC 3339 with offset, RFC 1123, `01/02/2006`, date-only, naive vs aware, empty), and `score` near a UTC day boundary.
- **Scoring:** `score` with a fixed `today`: overdue, due exactly in 7 days and 8 days, mood match (case-insensitive), and length thresholds at exactly 250 and 400 pages.
- **Limits:** a 10,000-row CSV, 10,001 rows, a 2 MB upload and 2 MB + 1, a non-UTF-8 file, a duplicate header, a BOM, ragged rows.
- **Property tests:** no randomized tests for ranking invariants.
- **Fuzzing:** none. The parsers are the untrusted-input surface.

## Task
1. Split `app_test.go` into `ranking_test.go`, `imports_test.go`, `store_test.go`, `web_test.go`, `sync_test.go`, with shared helpers in `helpers_test.go`. Existing assertions are kept, only moved; names stay the same so history is traceable.
2. Add tests for every gap above using the standard library only: table-driven tests with `t.Run`, seeded `math/rand/v2` (`rand.New(rand.NewPCG(seed, seed))`) for generated cases, `t.TempDir()` for databases, `httptest` for HTTP, no network. Add `t.Parallel()` where tests are independent and safe, and keep the suite under ~10s with `-race`.
3. Ranking invariants over a few hundred seeded random queues: the output is a permutation of the input IDs; pinned books keep their slots; every prerequisite precedes its dependents (or `Propose` returns an error and the input genuinely has no valid order, which you can establish with a brute force for ≤7 books); `Propose` is deterministic across repeated calls; it does not mutate its input.
4. Native fuzz targets (`func FuzzParseCSV`, `FuzzParseRSS`, `FuzzParseTime`) with a small seed corpus built from `examples/*.csv` and edge cases. Properties: no panics; records returned always have non-empty title/author within limits and a valid status; parsed times round-trip through `parseTime` as canonical. Run each for ~30 seconds locally and report any crashes (commit failing inputs under `testdata/fuzz/`). CI should run only the seed corpus (normal `go test`).
5. Report coverage before (72.1%) and after, list uncovered statements worth covering, and `go test -race -count=1 ./...` timing.

## Acceptance criteria
- `go test -race ./...` passes in under ~10s. `go vet ./...` and `gofmt -l .` are clean.
- Coverage is meaningfully higher and both numbers are reported. Do not chase 100%: skip generated or trivially unreachable error branches.
- If a new test reveals a real bug, do not hide it. Write the failing test, report the bug, and either fix it if trivial or mark the test with `t.Skip("known bug: …")` and a pointer in the report.
- Do not add test dependencies (no testify, no gomock).
