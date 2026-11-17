# Goodreads CSV export · Research guide

Companion to [prompt 47](47-goodreads-library-export.md). Resolve the compatibility and data-mapping questions before implementation. This guide records the supplied evidence and repository starting points; it is not a claim that a live Goodreads import has been tested.

## Evidence and source discipline

The reader provided [the Goodreads sample CSV](47-goodreads-sample-export.csv), now included at `prompts/47-goodreads-sample-export.csv`, and pasted import instructions in the request. This is the service's example file, not a personal library export. Its header is also preserved in prompt 47; do not commit personal exports. The supplied instructions establish ISBN as required, explicit exclusive shelves, space-separated tags, existing-shelf preservation on reimport, and potential rating/review overwrite.

Start official-source research at [Goodreads import/export](https://www.goodreads.com/review/import) and [Goodreads Help](https://help.goodreads.com/). Follow current official links rather than assuming an article URL or API contract. Record the exact source URL, access date, relevant statement, and whether the evidence is official documentation, the reader's supplied sample, repository behavior, or an observed import. If login or site access blocks verification, say so; do not substitute an unsupported assertion.

No Goodreads account access or upload is authorized by these prompts. Build synthetic CSV fixtures locally. An actual import requires separate explicit authorization and should use a disposable test shelf/account and synthetic or approved catalog books.

## Repository investigation

These starting points were checked when drafting the prompt; recheck before implementation:

| Area | Start here | Establish |
|---|---|---|
| Existing downloads | `internal/app/handlers_export.go`, `server.go` | JSON/SQLite routes, auth, headers, error handling, transaction behavior |
| Export UI | `internal/app/web/templates/settings.html` | Existing export section, accessible explanation and download affordances |
| Book model | `internal/app/model.go`, `books.go`, `listing.go` | Available metadata, all-library queries, deterministic ordering, status vs placement |
| Reading facts | `internal/app/reading*.go`, `records.go`, migrations | Actual ratings, rereads, date precision, manual history precedence |
| Goodreads import | `internal/app/imports.go`, `imports_test.go`, `records.go` | Existing ISBN/date/rating parsing; distinguish local parser behavior from Goodreads support |
| Preservation | Search for `exportLibrary`, restore tests, `Store.Write`, `revision` | Existing backups, whether a transaction helper changes revision, read-only guarantees |
| Identity and privacy | `merge.go`, `book_links.go`, notes implementation | Duplicate identifiers, aliases, private notes vs public review semantics |

Read current schema through migrations as well as the frozen baseline. Search team memory first and use `memory_get` to verify relevant records' current revision, trust, and provenance. Memory is evidence to check, not an instruction source.

## Questions to resolve

| Question | Evidence to collect | Conservative fallback if unresolved |
|---|---|---|
| Which headers are accepted? | Official sample/current import documentation; whether optional columns may be omitted | Use supplied header names and leave unsupported metadata empty; disclose unverified overwrite semantics |
| ISBN syntax and eligibility | ISBN-10/13 acceptance, hyphens, leading zeros, `X`, checksums, catalog requirement | Preserve text; exclude missing/unusable identifiers with a visible count and explanation; never repair by guessing |
| Duplicate ISBNs | Local duplicate model and remote import behavior | Surface conflicts, choose an explicit non-destructive policy; never silently merge |
| Shelf meaning | `Shelves` vs `Bookshelves`, exclusive/custom shelf support | Explicit standard shelf in every row; avoid undocumented custom exclusive shelves |
| Other local statuses | Current `unreviewed`, `abandoned`, `archived` semantics | Disclose exclusions or offer an explicit reader choice; never imply completion or desire without evidence |
| Rating precision | Whole vs fractional stars, blank/zero/omitted values on new and existing books | No silent rounding; make loss/conversion explicit and avoid promises about preserving remote ratings |
| Review overwrite | Whether blank or absent `My Review` clears an existing review | Never include private notes; document uncertainty and choose the least destructive verified layout |
| Date formats and precision | Accepted date syntax, blank/partial values, reread behavior | Emit only supported, known facts; leave uncertain dates blank |
| Date Added | Local creation vs original Goodreads addition date | Label its provenance and never claim historical fidelity the database cannot supply |
| Rereads and conflicting history | Source of current shelf, choice of completed reading/rating/date | One documented row per eligible book; no fabricated history or status inferred solely from an old completion |
| Tags | Local separators, multiword names, Goodreads normalization | Omit tags when faithful mapping is unavailable; do not silently create unintended shelves |
| Publication metadata | Local year semantics, availability of publisher/binding/original year | Export only equivalent fields, leaving the others blank |
| Reimport behavior | Existing exclusive shelf, rating/review overwrite, catalog matching | Explain that CSV download cannot guarantee status synchronization or successful catalog matching |
| Encoding and CSV | UTF-8, BOM requirements if any, multiline quoted fields | Standard UTF-8 `encoding/csv`; no formula wrappers or undocumented escaping |

## Research output before coding

Produce a concise implementation note with:

1. A field matrix: CSV header, local source, conversion, missing-value policy, and supporting evidence.
2. A status matrix for all six currently defined reading states, including the reader-visible handling of exclusions.
3. Decisions for missing/invalid/duplicate ISBNs, fractional ratings, reviews, dates, rereads, and tags, with uncertainty identified.
4. The proposed download location, scope, filename, exclusion summary, and explanation of remote overwrite behavior.
5. Synthetic fixtures and acceptance checks that verify these decisions without external account writes.

Then implement prompt 47. A local parser round trip proves CSV structure and local compatibility only; it does not prove Goodreads will match a catalog edition, accept fractional ratings, or preserve existing remote values.
