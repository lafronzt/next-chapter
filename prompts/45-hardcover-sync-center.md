# 45 · Hardcover sync center

Needs 39–43. Do last among the core set; it ties the pieces into one calm place.

## Goal
One page, `/hardcover`, that answers: is it connected, what would change in each direction, what conflicts need a decision, and what happened last time. It should make two-way sync feel safe, not magical.

## Design
1. **Status block:** connected user, scopes seen, last test, rate-limit remaining (from response headers), sync switches (pull, push, progress) each off by default.
2. **"Check for changes" (one button, never a timer):** runs an incremental pull (41, `updated_at` high-water mark) and a local diff against `hardcover_sync` (42), then shows three groups: *Hardcover is ahead* (offer pull), *Local is ahead* (offer push), *Both changed* (conflict). Each row has a single reader decision; "Apply selected" uses the plan flow with a revision check. Fetching costs requests, so show the estimated number up front ("about 3 requests; 4,812 left today").
3. **History:** the last N syncs with counts and per-book detail (what moved, direction, when), capped and prunable, no review text and no notes. Undo is a *local* undo through the existing snapshot; the page says plainly that Hardcover changes are not automatically undone and offers the reverse change as a new previewed push.
4. **Unlinked and unmatched:** a list of local books with no Hardcover link and Hardcover books with no local match, each with a link/add action from 40/41.
5. **Disconnect and delete:** "Disconnect" removes the link row and sync history, keeps every book, and leaves Hardcover untouched. Add a "Forget Hardcover ids" action (previewed) that clears `hardcover_*` columns. Both are reflected in export/restore.
6. **Failure honesty:** token expired or reset (401), missing scope (403 with the scope named and a link to hardcover.app/account/api), rate limited (show retry time), Hardcover down (503, nothing changed). No red stack traces, no retry loops, no partial apply left hidden.
7. **Optional scheduled check (stretch, off by default):** if the operator sets `HARDCOVER_SYNC_INTERVAL`, run a *read-only* "Check for changes" in the background job runner from `jobs.go` and only surface a notice ("3 changes waiting on Hardcover"). It never applies anything. Respect the budget and stop for the day on a daily-limit 429.

## Acceptance criteria
- Handler tests for each group, conflict decisions, apply, stale revision, disconnect (books untouched), forget ids, unlinked lists, and each failure banner.
- Request-count assertions: a check costs at most the stated number of top-level fields; a daily-limit 429 stops further calls.
- Background check (if built) applies nothing and is a no-op when the switch is off; a11y and pwa tests pass; desktop and phone layouts in light and dark.
- README: the whole integration in one section, including what Hardcover sees, the scopes, rate limits, what is never synced (reviews, notes, order, loans, AI data), and how to disconnect.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
