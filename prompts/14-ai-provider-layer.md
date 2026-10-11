# 14 · Opt-in AI provider layer (Claude or OpenAI)

Shared plumbing for prompts 15 and 16. Do 13 first. Do 02 and 06 first if you can: this adds the app's second outbound network path and a stored secret.

## Context and the privacy promise
The README currently says: "No font, analytics, cover-image, or AI provider receives reading data. The only outbound app request is opt-in Goodreads RSS." The project's identity is self-hosted and reader-owned. This work deliberately changes that sentence, so the design must make the change small, explicit, and reversible:

- **Off by default.** Nothing leaves the server unless the operator configures a provider *and* the reader turns AI on in Preferences.
- **Bring your own key.** No hosted proxy, no paid service of ours. The operator supplies the key.
- **Show before you send.** For every AI action, the reader can see exactly what will be sent (a "What gets shared" preview) before confirming. Nothing is sent in the background, on a timer, or on page load.
- **Minimum data.** Send only what the task needs (see 15 and 16): titles, authors, tags, page counts, series, interest, rating, finish year. Never send: library-card or loan details, the password, session data, review text (unless the reader opted in to storing it and ticks a per-request box), ISBNs/Goodreads IDs/user IDs, or other metadata that identifies the reader.

## Task
1. **Provider interface** in a new file `internal/app/ai.go`:
   ```go
   type Completer interface {
       Complete(ctx context.Context, req AIRequest) (AIResponse, error)
   }
   ```
   with `AIRequest{System, User string; MaxTokens int; Schema json.RawMessage}` and `AIResponse{Text string; InputTokens, OutputTokens int}`. Implement two providers with `net/http` and `encoding/json` only (no vendor SDK, to keep the dependency list unchanged):
   - **Anthropic Messages API** (`POST https://api.anthropic.com/v1/messages`, headers `x-api-key`, `anthropic-version`).
   - **OpenAI** (`POST https://api.openai.com/v1/responses` or `/v1/chat/completions`; pick the current recommended endpoint from OpenAI's docs at implementation time).
   Check each vendor's current docs for endpoint, headers, structured-output/JSON mode, and error shapes; do not rely on memory. Model names must come from configuration, not be hard-coded as the only option (suggested defaults go in `.env.example` and README with the date they were checked).
2. **Configuration:** env vars `AI_PROVIDER` (`off`|`anthropic`|`openai`, default `off`), `AI_API_KEY` (or `ANTHROPIC_API_KEY`/`OPENAI_API_KEY`), `AI_MODEL`, optional `AI_MAX_MONTHLY_REQUESTS` (default 100) and `AI_BASE_URL` (HTTPS only; used by tests). Read the key from env only: never store it in the database, never include it in backups/exports, never render it, never log it or request/response bodies. Refuse to start with a clear message if `AI_PROVIDER` is set without a key or model. Document Kubernetes `Secret` usage in `deploy/kubernetes/`.
3. **Safety rails:**
   - Fixed hosts per provider (no user-supplied URL besides the test override, which must be rejected unless `https://127.0.0.1…`/`httptest`), a 60s timeout, response size limit, no redirects, and `User-Agent: NextChapter/<Version>`.
   - Per-month request counter in `meta` with the cap above; show usage (requests and tokens) in Preferences. Exceeding it returns a clear user-facing error.
   - Strict parsing of model output: request JSON that matches a schema, decode with `DisallowUnknownFields`, validate every field (IDs must be in the set that was sent; ratings 1–5; no duplicates), and discard the response on any violation. **Treat model output as untrusted input**: it can only propose, never write to the database directly.
   - Prompt-injection hygiene: book titles, tags, and reviews are data from imported files. Put them in a clearly delimited data block, tell the model they are untrusted data and not instructions, and rely on schema validation (above) as the real defence.
   - Concurrency: one AI request at a time (`TryLock`), never inside `Store.Write` (the database has one connection; holding it during a network call would freeze the app). Read inputs in a short read, call the provider, then open a write only to save the result.
4. **Preferences UI:** an "AI assistance" section showing provider/model (read-only, from env), an on/off toggle (stored in `meta`), the monthly usage, the data-sharing summary in plain language, and a "test connection" button that sends a fixed harmless prompt with no library data.
5. **Egress and CSP:** the CSP applies to the browser, not the server, so no change is needed there. Update `SECURITY.md` (new outbound hosts, key handling, what is and is not sent) and the README sentence above.

## Acceptance criteria
- Tests with `httptest` servers standing in for each provider (no real network, no real key): request shape and headers per provider; success parse; 401/429/5xx and malformed bodies map to friendly errors without leaking the key or body; oversized body is rejected; monthly cap enforced; AI is inert when `AI_PROVIDER=off` or the reader toggle is off (assert zero outbound requests, using a transport that fails the test if called); invalid/unknown-ID/duplicate-ID responses are rejected; the key never appears in logs (capture the logger), pages, export, or backup.
- Existing tests pass; `go vet`, `gofmt` clean; no new Go dependencies (justify any exception).
