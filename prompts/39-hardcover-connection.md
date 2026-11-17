# 39 · Hardcover connection and API client

Foundation for 40–46. Needs 05 (migrations). Do not start 40+ before this lands.

## Goal
Let a self-hoster connect Next Chapter to their own [Hardcover](https://hardcover.app) account, and give the rest of the app one safe, rate-limited, testable client for the Hardcover GraphQL API. This prompt moves no book data. It adds config, a client, a connection status page, and the privacy wording.

## What the Hardcover API is (verified against docs.hardcover.app, 2026-11)
- One GraphQL endpoint, `https://api.hardcover.app/v1/graphql` (Hasura-style `where` / `order_by` / `limit` / `offset`). Beta: "heavily in flux", tokens may be reset without notice. Write defensively and keep the surface small.
- `Authorization: Bearer <token>`. Tokens are either a personal access token (PAT) from hardcover.app/account/api, created with an expiry and **scopes**, or an OAuth access token (`hc_at_…`, with `hc_rt_…` refresh token). Pre-August-2026 PATs act as `all`.
- Scopes used by this integration: `read:me`, `read:library`, `write:library` (user_books, user_book_reads, reading journal), `read:catalog` (books, editions, series, search), and, only for 46, `read:lists` / `write:lists`. Request nothing else. Never ask for `all`.
- Limits (Free plan): 5,000 requests/day, 60/minute, **burst 10**. Each *top-level field* costs one request, aliases count separately, nested fields are free. A request with more top-level fields than the burst gets `403 request_exceeds_capacity`. `user_book_reads` and similar cap at 1,000 rows per request (a larger `limit` is silently cut; page until a short page). Query timeout 30 s, **search timeout 2 s**. Responses carry `RateLimit` / `RateLimit-Policy` and legacy `X-RateLimit-*` headers; `429` carries `Retry-After`.
- Errors: 400 bad query, 401 bad/expired token, 403 missing scope or disallowed op, 408 timeout, 429, 503 (safe to retry).
- Terms that shape the design: no training publicly available or commercial LLMs on API data; a commercial or public product may not use other users' data. This app is single-reader and private, so only the reader's own library is touched. Do not read other users' data, ever. Hardcover images are user-uploaded; see 44 before showing covers.
- OAuth exists (PKCE required for every client; confidential-client flow needs a client id and secret). Docs say "never ask a user to paste a PAT into your app".

## Design (decide, then follow)
1. **Off by default; nothing runs on a timer, page load, or import.** Every Hardcover call is a reader action (a button on a preview) or an explicitly enabled manual sync. The app must work fully with Hardcover unconfigured and must never require a paid plan.
2. **Credentials live in the environment, not the browser.** Follow `AI_API_KEY`: `HARDCOVER_API_TOKEN` (PAT or OAuth access token) in `.env`, documented in `.env.example`, never stored in the database, never rendered, never logged, redacted from errors. This satisfies the docs' warning because the operator and reader are the same person and the token never touches a page. Add a Preferences switch ("Hardcover") in the Privacy section from 31, off by default. Write the OAuth confidential-client flow only as a documented follow-up; do not build it here.
3. **One client, `hardcover.go`.** `type hardcoverClient` with an injectable `*http.Client` and base URL (tests use `httptest`). Fixed host, no redirects followed, short dial timeout, 30 s request timeout (3 s for `search`), response size limit, `User-Agent: NextChapter/<Version> (+self-hosted)`. A generic `do(ctx, query, vars, &out)` that decodes `{data, errors}` and maps HTTP and GraphQL errors to typed errors (`errAuth`, `errScope{need}`, `errRateLimited{retryAfter}`, `errUnavailable`, `errTimeout`).
4. **A budget, not a hope.** A small token bucket seeded from the `RateLimit` response headers (fall back to 10 burst / 60 per minute / 5,000 per day), counting top-level fields per request. Split work so no request exceeds the burst. On `429`, honor `Retry-After` once, then stop and report; never loop. Persist nothing but a "last known remaining" figure shown on the status page.
5. **Network calls never run inside `Store.Write`** (single connection; see README traps). Fetch first, then apply in one transaction.
6. **Verify the schema at connect time.** Hardcover is in flux. Add a "Test connection" action that runs `me { id username }` plus a tiny introspection check (`__type(name:"user_books")`, `__type(name:"user_book_reads")`) and reports missing fields or scopes in plain language. Keep a checked-in `testdata/hardcover/` of recorded response shapes that the unit tests parse, so a drift is a test failure, not a production surprise.
7. **Link record.** Migration (next available version; backup and rollback notes in the file, README, and PR; raise `maxRestoreSchema`; add upgrade test; check JSON restore): table `hardcover_link (id INTEGER PRIMARY KEY CHECK(id=1), hardcover_user_id INTEGER NOT NULL, username TEXT NOT NULL, linked_at TEXT NOT NULL)`. Store the user id and username only. Include it in export, and on restore **drop it** (a restored database must be re-linked with the current token) with a notice.

## Task
- `hardcover.go` client, errors, budget; `handlers_hardcover.go` with `GET /hardcover` (status: configured or not, user, scopes seen, rate-limit remaining, last test result) and `POST /hardcover/test` and `POST /hardcover/disconnect` (deletes the link row; changes no books). Add a nav entry under More; follow 19/31 patterns, CSRF, no inline script or style, and check desktop and phone layouts in light and dark.
- Config parsing in `server.go` with clear startup errors (token set but the feature switch off is fine; switch on and token missing is a visible status, not a crash).
- README: a "Hardcover" section: what is sent (a bearer token, queries naming only the reader's own book ids, ISBNs, titles and authors for matching), to whom, how to turn it off, the scopes to grant, rate limits, beta caveats, and that Hardcover is optional. Update the privacy promise the way 14 and 32 did.

## Acceptance criteria
- Client tests against `httptest`: success, 400/401/403 `insufficient_scope` (scope named in the error), 403 `request_exceeds_capacity`, 408, 429 with `Retry-After`, 503, GraphQL `errors` with HTTP 200, oversized body, malformed JSON, redirect refused, header-driven budget, token never present in any error string or log line, `User-Agent` set.
- Hardcover off or no token means zero outbound requests (assert with a transport that fails the test).
- Migration test from the previous version; export/restore round trip; restore drops the link.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean; `go mod tidy` leaves `go.mod`/`go.sum` unchanged (standard library only).
