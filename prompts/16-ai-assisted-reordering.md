# 16 · AI-assisted queue reordering

Needs 14 (provider layer), and benefits from 13 and 15. Do 01 first if you want large queues to feel fast, and 07 so the plan page copes with big queues.

## Goal
Let the reader ask for a suggested reading order ("Rethink my queue", "Find its place") that uses AI to weigh things the deterministic score cannot: mood and context in the reader's own words, thematic variety, pacing, and what they just finished.

## Non-negotiable invariants
- **Never silently reorder.** AI output creates a **plan** (the existing preview → confirm → apply flow with revision check and undo). Nothing is applied automatically, on a schedule, or on a Goodreads sync. New synced books stay "Needs placement" until the reader acts.
- **Hard constraints stay deterministic and win.** Pins keep their slots and series prerequisites precede their dependents, enforced by the existing `Propose` logic, not by the model. The AI never writes order directly.
- **Explainable.** Each moved book shows a reason. Reasons from the model are labelled as AI-generated; the existing score reasons are shown alongside.
- **Works with AI off.** Everything in the current flow behaves exactly as today when AI is disabled.

## Design
1. **Two-stage ranking.** Compute deterministic scores and constraints as today. Send the model a *bounded* candidate set (for example the top 60 books by score plus the candidate and pinned books, not 5,000 titles) with: id (an opaque small integer, not the database ID or Goodreads ID), title, author, tags, pages, series info, interest, estimated rating if present, library status (only "borrowed/ready, due in N days", no source/account details), and the reader's optional free-text intent for this session (for example "something light after a heavy book, I have a long flight Friday"). Also send the last ~5 finished books for variety context.
2. **Model output:** JSON `{"order": [opaque ids...], "reasons": {"id": "short text"}}` over the *candidate subset only*. Validate (prompt 14): every id present exactly once, no unknown ids, reasons length-capped.
3. **Merge with constraints:** feed the model's order into the constraint solver as a *preference* (it replaces the score as the sort key within the allowed space). If the proposed order violates pins or series, repair it deterministically (move the minimum number of books) and flag the repaired items in the preview. Books outside the candidate subset keep their current relative order after it. If the model output is invalid, fall back to the deterministic proposal and tell the reader plainly.
4. **Insert mode:** for "Find its place", ask the model only to choose a slot (an index range) for the one candidate with a reason; validate against `Propose`'s valid slots, never outside them.
5. **Preview UI:** extend the plan page to show deterministic vs AI-suggested order side by side or as a toggle ("Use my usual ranking" / "Use AI suggestion"), movement arrows, reasons, the **"What gets shared"** summary, model name, and token usage. Applying uses the existing reorder + undo.
6. **Reader intent box:** an optional text field on the "Rethink my queue" form. It is sent to the provider only when AI is on and is shown in the "What gets shared" list. Do not store it beyond the plan's one-hour lifetime.

## Task
- `rerank.go`: build the bounded request, call the provider, validate, merge/repair, return `[]Suggestion` in the existing `Plan.Rows` shape with a `Source` of `"ai"`/`"rules"`. Extend `Plan` (add fields, keep JSON compatible) and teach `plan()` to apply both identically.
- Run the AI call outside `Store.Write` (per 14), then save the plan in a short write; the plan's stored revision still guards staleness, and a library change during the AI call must produce a "library changed; regenerate" message rather than a stale apply.
- Document in README: how it works, what is sent, cost expectations (a rough tokens-per-request figure from your measurements), and that rules-only mode is the default.

## Acceptance criteria
- Tests with a fake provider: valid AI order is merged and respects pins/series; an order that breaks a pin or series is repaired and flagged; invalid JSON, duplicate ids, or unknown ids fall back to rules with a visible message; prompt-injection text in a book title ("ignore previous instructions…") cannot change anything beyond the validated order (assert the plan equals what the validator allows); the request contains only whitelisted fields and opaque ids; a revision change during the call blocks apply; AI off ⇒ results identical to today's `Propose` and zero outbound requests; applying an AI plan is undoable with the existing undo.
- Differential/property test: for random queues, pins, and series, any AI order (random permutations as fake provider output) yields a result where pins hold and prerequisites precede dependents.
- Existing tests pass; `go vet`, `gofmt` clean.

## Out of scope
Automatic or scheduled reordering, AI-written book metadata edits, embeddings or a vector store, sending cover images, and any provider-hosted memory of the reader.
