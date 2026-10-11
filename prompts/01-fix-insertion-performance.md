# 01 · Insertion ranking performance

Lower priority since the Go rebuild. Do this after 05, 02, and 06.

## Context
`Propose` in `internal/app/model.go` powers "Find its place" (insert mode, `candidateID != 0`) and "Rethink my queue" (full reorder). It is called from `makeSuggestion`, which runs **inside `Store.Write`**. With `SetMaxOpenConns(1)`, a slow `Propose` blocks every other request, including `/healthz`, whose container `HEALTHCHECK` times out after 5s.

## Measured (Go, `4bafb11`, synthetic books, 10% in series, no pins)

| Queue size | Insert | Full reorder |
|-----------:|-------:|-------------:|
| 200  | 5 ms   | 1 ms   |
| 500  | 24 ms  | 8 ms   |
| 1000 | 75 ms  | 28 ms  |
| 2000 | 313 ms | 116 ms |

Growth is roughly quadratic. This is fine for typical use but it is the dominant cost of the app's heaviest write, and 5,000 to-read books would take around two seconds under the write lock.

## Where the time goes
1. `prerequisites(b, books)` scans all books for every book, so building `deps` is O(n²) with an `strings.EqualFold` per pair. Group indexes by lower-cased series name once and only compare within a series.
2. Insert mode, inside `for i := 0; i <= len(rest); i++`: builds a fresh `trial` slice and a `pos` map per slot (O(n) each), re-checks every pin, and loops over all of `rest` to look for dependents.
3. Full reorder: `eligible()` scans all books per slot, and the "urgent" search runs a nested pin scan per slot.

## Task
1. Make insert mode O(n) or O(n log n) with output **identical** to today's.
   - Precompute the candidate's prerequisites and dependents once. Dependents are the books whose `deps` contain the candidate.
   - Replace per-slot trial slices with index arithmetic. For slot `i` in `rest`, the slot is valid when every pin still lands on its book (pins at or after `i` shift by +1), every prerequisite sits at index `< i` in `rest`, and every dependent sits at index `>= i`. Because validity is monotone in `i` for each of those constraints, the valid slots form an interval; compute its bounds, then pick the slot nearest `ideal`.
   - Keep the tie-break exactly: first minimal `|i - ideal|`, lowest `i` wins (strict `<` over ascending `i`).
2. Build `deps` with a series index so cost is proportional to series sizes, not n².
3. Look at full-reorder mode. Fix the per-slot rescans only if the change is simple and the output stays identical (including tie-breaks: first in queue order wins equal scores).
4. Keep error strings unchanged: `choose an unpinned book in your queue`, `pins and series order leave no valid position; …`, and the pinned-conflict messages.

## Acceptance criteria
- All existing tests pass unmodified.
- New tests in `internal/app`:
  - A seeded randomized differential test (`math/rand/v2` with a fixed seed) against a reference oracle. Copy the **current** `Propose` into the test file as `proposeReference` before changing it. Compare on queues of 5–40 books with random pins, series (including duplicate series numbers and mixed-case series names), scores, and candidates. Results and error strings must match exactly, including which inputs error.
  - A scale guard: insert into a 2,000-book queue in well under 250 ms on CI (use a generous bound so it is not flaky), and full reorder under the same bound.
- `go test -bench` benchmarks for 200/500/1000/2000/5000 books, and a before/after table in the report.

## Out of scope
Changing scoring weights, constraint semantics, or moving `Propose` out of the transaction (that interacts with plan revision checks; mention it in the report as an option if timing still matters).
