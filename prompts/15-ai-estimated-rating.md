# 15 · Estimated rating ("what might I give this?")

Needs prompts 13 (history) and 14 (provider layer). Do 12A first so ratings flow into `interest`.

## Goal
For a book in the queue, show an **estimated rating the reader might give** (1–5, one decimal allowed), a confidence level, and a short plain-language reason, based on their own rated history. It is an aid for deciding, not a fact.

## Design (decide, then follow)
Do not make the model do all the work. Combine a deterministic baseline with the AI's judgement so the result is explainable and works with AI off:

1. **Baseline (no AI, always available):** compute from `reading_log` + books: the reader's average rating, their average rating for books sharing tags/series/author with the candidate, and the Goodreads `avg_rating` as a weak prior. Show "Based on N rated books" and the factors. If fewer than ~10 rated books exist, say the estimate is low-confidence rather than guessing.
2. **AI refinement (opt-in, on request):** send the model a *compact* taste profile plus the candidate and ask for an adjusted estimate and a reason. Do not send the whole library on every request. Send, at most: up to ~40 representative rated books (title, author, tags, pages, series, the reader's rating, finish year; choose highest- and lowest-rated plus nearest by tag overlap), aggregate stats, and the candidate's title/author/tags/pages/series/average rating. No reviews unless the per-request box is ticked.
3. Output schema: `{"estimate": number 1..5, "confidence": "low|medium|high", "reasons": [string,string,string]}`; reasons capped in length; validated per prompt 14. Show baseline and AI value side by side, labelled; never replace one silently with the other.

## Task
- Add `estimate.go` with the baseline as a pure function (table-driven tests) and the AI call behind the `Completer` interface.
- UI: on each book (queue row and edit page) a "Predict my rating" action (POST, CSRF, no revision bump because nothing in the library changes). Show a **"What gets shared"** confirmation listing the exact rows that will be sent, with a Send button, before any AI call (per 14). Cache the result in a new table `rating_estimates(book_id, basis_fingerprint, baseline REAL, ai REAL, confidence, reasons, model, created_at)` via migration; the `basis_fingerprint` is a hash of the inputs, so the estimate is marked stale when the reader's history changes materially. Cached estimates are shown without any network call.
- Optional, off by default: use the estimate as a score input in `score()` (a small bonus/penalty, shown as a reason like "Estimated 4.3★ for you"). Keep it deterministic: ranking uses the *stored* estimate, never a live call. This must not change ordering for anyone who has not generated estimates.
- After the reader finishes a book and rates it, record the estimate vs actual in a small "How accurate was I?" line (mean absolute error) so the reader can judge whether to trust it.

## Acceptance criteria
- Tests: baseline math on seeded fixtures; low-confidence behavior with few ratings; the request body sent to a fake provider contains only the whitelisted fields (assert absence of ISBN, Goodreads IDs, review text, library loan fields); a response with an out-of-range estimate or extra fields is rejected; the cached estimate is reused with no outbound call and marked stale after history changes; AI off → baseline only and zero outbound requests; scoring with no estimates is byte-for-byte identical to today's ranking (regression test).
- README: how the estimate is made, what is sent, and its limits (it can be wrong; Goodreads average is a weak prior).
- Existing tests pass.
