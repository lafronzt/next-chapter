# 06 · Make the poller and sync resilient

## Context
`internal/app/sync.go` fetches a public Goodreads RSS feed on demand (`POST /sync` → `syncNow`) and hourly from `(*App).Poll`, a goroutine started in `cmd/next-chapter/main.go` and stopped through the signal context. `syncOnce` serializes fetches with `syncMu.TryLock()` and keeps network I/O outside any DB transaction. `App.fetch` is already an injectable field (tests can stub it); `FetchGoodreads` itself is not testable.

## Problems found (verified at `4bafb11`)
Gone since the Python version: a failed status write can no longer kill the poller (errors are logged and the loop `continue`s). Still open:
1. **A panic kills the process.** Nothing in `Poll` or `syncOnce` recovers. A panic in RSS parsing or import code inside the poller goroutine terminates the whole server, not just the poller.
2. **One message for every failure.** `syncNow` returns "Goodreads could not be checked. Verify your public shelf…" for *every* error, including deliberate ones from `syncOnce`/`FetchGoodreads`: "save your Goodreads user ID first", "connection settings changed; try again", "a Goodreads check is already running", and bad shelf/user-ID format. The poller records "Feed unavailable. Check that your shelf is public…" in `last_sync` for the same set, so a lock-contention skip is reported as an outage.
3. **Redirects are always rejected.** `CheckRedirect` returns an error for any redirect ("use CSV import"). Goodreads sometimes redirects (for example to a canonical path). Following redirects only to `https://www.goodreads.com/…`, with a hop limit and nothing else, would preserve the SSRF protection.
4. **No backoff or escalation.** `last_poll` is written before the attempt, so a failure means a one-hour wait; a persistent failure retries hourly forever and the UI shows only the last message.
5. **Untestable fetch.** The URL and HTTP client are hard-wired, the 1-minute ticker is the only entry to the poll logic, and `time.Now()` is called inline, so none of `FetchGoodreads`, redirect rules, or due-time logic can be tested without sleeping or the network.
6. **Poll errors are opaque.** The failure log line contains no error class, so an operator cannot tell a timeout from a parse error. (Do not log the full URL or user ID at INFO.)

## Task
1. Wrap each poll iteration in a function with `defer recover()` that logs the panic value and carries on. Apply the same guard to the manual path if it can panic inside a handler (the HTTP server already recovers handler panics; keep that).
2. Introduce sentinel/typed errors in `sync.go` (for example `errSyncConfig` for settings problems, `errSyncBusy`, `errFeedUnavailable` wrapping network/status/parse failures). `syncNow` shows the specific message for config/busy errors and the outage text only for feed errors; the poller records the matching text in `last_sync` and does not count a busy skip as a failure. Keep user-facing texts free of URLs and IDs.
3. Follow redirects only when the target scheme is `https` and the host is exactly `www.goodreads.com`, up to 3 hops; reject everything else, including a redirect that downgrades scheme, changes host/port, or carries credentials. Keep the 15s timeout and the response size limit.
4. Make the logic testable: give `FetchGoodreads` an injectable `*http.Client` (or `http.RoundTripper`) and base URL held on `App`/a small `feedClient` struct (default production values). Extract `(a *App) pollIfDue(ctx, now time.Time)` containing everything one tick does, so tests drive it with a fake clock and a stub `fetch`.
5. Exponential backoff after consecutive failures: 1h, 2h, 4h … capped at 24h, reset on success, with the failure count, last error time, and next attempt time stored in `meta` (written in the same transaction as the status where practical). Write `last_poll` only after the attempt outcome is known, or compute "next attempt" from the failure state so a crash mid-attempt does not skip a retry. Show last error time and next attempt on the Preferences page (extend `settings.html` and the `settings` handler).

## Acceptance criteria
Tests with `httptest` TLS/redirect servers or a stub `RoundTripper` (no real network):
- Success path adds books and records `last_sync` (OK).
- A redirect to another host, to `http://`, or to a different port is rejected; a redirect to `https://www.goodreads.com/…` is followed; a fourth hop is rejected.
- A non-200 status, an oversized body, and unparseable XML each yield a feed error and the outage message.
- `pollIfDue` is a no-op when disabled or not yet due, and attempts when due, with a fake clock.
- Backoff doubles on consecutive failures up to the 24h cap and resets on success.
- A panic inside the stubbed `fetch` during `pollIfDue` is recovered and logged, and the next call still works.
- "Settings changed during fetch" and "already running" surface as their own messages through `syncNow`, not the outage message.
- Existing tests pass. No test touches the real network.

## Out of scope
Other feed sources or Goodreads writeback.
