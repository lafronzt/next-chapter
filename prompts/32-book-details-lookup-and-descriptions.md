# 32 · Book details lookup and descriptions

Needs 05 (migrations) and 15 (estimated rating). Do 27 first if you want the description shown on the read-only book page.

## Goal
Fill in missing book details, and a **description**, from a global book database when the reader asks, and let the rating estimate use that description. Today a Goodreads or library import gives title, author, and little else, so the estimate and the placement flow have almost nothing to judge a candidate by.

## Design (decide, then follow)
The built-in AI makes one tool-free request and the reader previews what is sent (14, 15). Keep that. Do not give the in-app model web search or MCP tools. Split the work instead:

1. **Lookup is deterministic and not AI.** A plain HTTPS client in the app queries a global database and returns facts. No model decides what is true.
   - Primary: **Open Library** (no key): `GET /isbn/{isbn}.json`, then the work at `/works/{id}.json` for `description` and `subjects`; `GET /search.json?title=…&author=…` when there is no ISBN.
   - Fallback: **Google Books** (`/books/v1/volumes?q=isbn:…` or `intitle:…+inauthor:…`, key optional).
   - Optional, only when the operator sets a key: **ISBNdb** (paid) and **WorldCat** (OCLC credentials). Never required; the app must work fully without them (`AGENTS.md`: no mandatory paid services).
2. **Opt-in, per action.** Nothing is looked up on a timer, page load, or import. The reader taps "Look up details" on one book, or "Look up details for these N books" on the import preview. A "What gets sent" line lists exactly the fields that leave the app: title, author, ISBN. Never review text, ratings, tags, or reading history. Add a Preferences switch ("Book lookup") in the Privacy section from 31, off by default, and update the README privacy promise.
3. **Fill, never overwrite.** A lookup proposes values. The reader sees a before/after for each field and confirms. Only empty fields are pre-selected; a value the reader typed or imported is never replaced silently. A description is stored only when the book has none.
4. **The AI can use what was found.** When a stored description exists, `buildEstimatePayload` includes it in the candidate (cut to 1,000 characters), shown in the existing "exact prompt" preview. It does not enter the fingerprint of the reader's history, but it does invalidate a saved AI estimate for that book, because the candidate changed. Update `estimateFingerprint` and its test accordingly.
5. **Untrusted data.** Database text can contain anything. Store it as plain text, strip markup, cap it at 2,000 characters, render it escaped, and put it only inside `<library_data>` in prompts (see `untrustedNotice` in `ai.go`).

## Task
- New file `lookup.go`: a small `BookSource` interface (`Lookup(ctx, query) (BookDetails, error)`), implementations for Open Library and Google Books, and a merge step that prefers the source most agree on for pages and year. Take an injectable `http.Client`/base URL so tests use `httptest`. Fixed hosts only, no redirects followed, a short timeout, a response size limit, and a descriptive `User-Agent` (`NextChapter/` + `Version`). Network calls happen outside `Store.Write`.
- Migration (after 05's framework, with backup and rollback instructions in the file and the PR): add `books.description TEXT NOT NULL DEFAULT ''`. Include it in export, JSON restore (accept older exports without it), merge (keep the target's, else the source's), and plans where fields are carried.
- Import: after the preview, an optional "Look up details" step that adds `description`, `pages`, `published_year`, `series`, `series_number`, and `tags` only where empty. It must stay transactional and idempotent; a second run changes nothing.
- UI: a "Details" block on the book page with the description, its source, and the lookup action. Extend existing templates and CSS; no inline script or style; full-page POST + CSRF + revision contract; check desktop and narrow mobile.
- Never reorder the queue as a side effect. Ranking is unchanged by a lookup.

## External agents
For readers who want an AI with web search and MCP servers (Claude Code or Desktop) to do deeper research, link `prompts/external-agent-book-lookup.md` from the README's AI section as an optional, outside-the-app workflow. Do not build that into the app.

## Acceptance criteria
- Tests: lookup against fake Open Library and Google Books servers (ISBN hit, title/author search, ambiguous result, 404, oversized body, malformed JSON, redirect refused); the request carries only title, author, and ISBN; fill-only-empty semantics; a typed description is never overwritten; import lookup is idempotent; lookup off means zero outbound requests; the estimate payload includes the description only when present, and a changed description makes a saved AI estimate stale; ranking output is byte-for-byte unchanged by a lookup; export and restore round-trip `description`, and an old export without it still restores.
- Migration test from a database at the previous version.
- README: what is sent, to whom, how to turn it off, the optional keys, and the limits (databases can be wrong or missing).
- `go test -race ./...`, `go vet ./...`, and `gofmt -l .` clean.
