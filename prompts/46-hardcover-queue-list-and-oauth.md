# 46 · Optional extras: publish the queue as a Hardcover list, and OAuth

Needs 39–45. Both parts are independent and optional. Do not start either without the reader's go-ahead.

## Part A · Queue as a Hardcover list
Hardcover has no reading-queue order, but it has **lists** (`write:lists`: `insert_list`, `insert_list_book`, `delete_list_book`, `delete_list`; `read:lists`). Offer a one-way, reader-initiated "Publish my next N books to a Hardcover list".

- Create or reuse one list named "Next Chapter queue", **private by default** (confirm the privacy field in `lists` via introspection first; a public list is a disclosure and needs an explicit choice in the preview).
- Only the top N (default 10) to-read books that are linked (40). The list's `position` / ordering semantics must be verified from the Lists schema doc before writing; if a list has no stable order, say so in the preview and rely on insertion order.
- Preview shows adds and removes; applying edits only entries this app created (store `hardcover_list_id` and per-entry ids in a table; migration; export/restore, droppable). Never touch the reader's other lists.
- One-way: reordering on Hardcover never changes the local queue. Refresh is a manual action. Needs `write:lists`, requested only when this feature is enabled.

## Part B · OAuth connection
Replace the env-var PAT with Hardcover's OAuth so access is short-lived, scope-limited, and revocable.

- Use the **confidential client** flow with PKCE (docs: "Getting Started with OAuth: Confidential Client"; PKCE is required on every client). Read `docs.hardcover.app/api/oauth/` for the authorize, token, and refresh endpoints and the app-registration steps; do not guess them. `HARDCOVER_CLIENT_ID` and `HARDCOVER_CLIENT_SECRET` come from the environment, never the database or a page.
- Callback route with a one-time `state` (bound to the session, short TTL) and the PKCE verifier kept server-side; reject mismatched state and replays. Redirect URI must match the one registered, so document the `COOKIE_SECURE`/reverse-proxy setup for the callback host.
- Store tokens encrypted at rest with `APP_SECRET_KEY` (AES-GCM, stdlib), access token `hc_at_…` plus refresh token `hc_rt_…` and expiry. Refresh before expiry under a mutex (single connection: do the HTTP outside `Store.Write`). On refresh failure, mark disconnected and surface the 45 banner.
- Request only the scopes the enabled features need; adding a feature that needs more scope sends the reader back through consent with a plain explanation. "Disconnect" revokes if the docs describe a revoke endpoint, otherwise says the reader must revoke in their Hardcover account.
- Keep the PAT path working. The app still must run with Hardcover unconfigured.

## Acceptance criteria
- A: fake-server tests for create, reuse, add, remove, privacy default, only-own-entries edits, a unlinked book skipped with a reason; queue order and ranking unchanged by publishing.
- B: tests for state mismatch and replay, expired code, refresh success and failure, concurrent refresh serialized, tokens never in logs or responses, encryption round trip, scope upgrade flow, PAT fallback.
- Migrations, export/restore (tokens are **not** exported), CSRF on every POST, a11y and mobile checks.
- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean.
