# 47 · Export the library to Goodreads CSV

## Goal

Let the reader download their Next Chapter library as a CSV they can upload to Goodreads. Keep the existing JSON export and SQLite backup. This is a reader-initiated file download, not an API integration or automatic upload.

Read [the research guide](47-goodreads-library-export-research.md) before implementing. Review `AGENTS.md`, `README.md`, `CONTRIBUTING.md`, `SECURITY.md`, and current team memory in `personal/next-chapter`; verify relevant memories against the code. Inspect the current branch rather than relying on older prompt status tables.

## Import contract supplied by the reader

The reader supplied [the Goodreads sample CSV](47-goodreads-sample-export.csv) and Goodreads import instructions. The sample is included in this repository at `prompts/47-goodreads-sample-export.csv`. Its columns, in order, are:

```csv
Title,Author,ISBN,My Rating,Average Rating,Publisher,Binding,Year Published,Original Publication Year,Date Read,Date Added,Shelves,Bookshelves,My Review
```

The sample has spaces around some headers and values; use clean header names. Do not copy the sample's numeric-looking ISBNs as validation examples: some have lost leading zeros. Use synthetic fixtures.

- ISBN is required; the book must exist in Goodreads' database.
- `Shelves` accepts `read`, `to-read`, and `currently-reading`. Blank defaults to `read`, so always populate it explicitly.
- Multiple tags or shelf names use spaces as separators.
- Reimporting a book does not change its existing exclusive shelf.
- Imported rating/review information can overwrite existing Goodreads values.
- CSV is preferred; quote commas and other CSV-sensitive text correctly.

Treat these as the supplied contract. Verify additional claims through official sources and identify anything that remains untested.

## Task

1. Add a clearly labeled **Export for Goodreads** action in the existing export area. Explain that the reader uploads the file manually, missing ISBNs may prevent export/import, existing Goodreads shelves may stay unchanged, and ratings/reviews can overwrite remote values. Keep the workflow usable without JavaScript and check desktop/mobile in light and dark themes.
2. Export the whole library, independent of pagination, search, active filters, and saved views. Choose a deterministic row order without changing local queue order. Explain excluded rows and their reasons before download; do not silently omit books. If no rows are eligible, show a useful empty state.
3. Use `encoding/csv`, UTF-8, a suitable CSV content type, and an attachment filename such as `next-chapter-goodreads.csv`. Preserve commas, quotes, Unicode, newlines, and ISBN leading zeros. Do not use spreadsheet formulas to preserve identifiers. Reuse existing authentication and error-handling conventions. Avoid returning an apparently successful partial file after query or serialization failure.
4. Use a valid stored ISBN as a string, reusing compatible normalization where available. Never guess missing digits or invent an ISBN. Resolve the policy for absent, malformed, and duplicate ISBNs using the research guide; report affected books and do not merge or rewrite them automatically. Do not promise Goodreads catalog acceptance merely because an ISBN is valid. No external catalog calls are required for downloading a CSV.
5. Map local `read` → `read`, `reading` → `currently-reading`, and `to-read` → `to-read`. Decide and document a conservative policy for `unreviewed`, `abandoned`, and `archived`; do not silently represent abandoned/archived books as completed or wanted. Never leave `Shelves` blank. Placement state, library loans/returns, and the existence of an old completed reading must not independently determine the current shelf.
6. Map only metadata with matching meaning. Leave unavailable fields blank. Preserve the distinction between publication year and original publication year. Use dates from corresponding stored facts, formatted for Goodreads; do not invent a finish date or confuse import time, checkout time, or progress timestamps with completion. Resolve reread selection and partial dates explicitly.
7. Research Goodreads' accepted rating precision before exporting the app's quarter-star reader ratings. Do not silently round them. Use only actual reader ratings; interest, predictions, catalog averages, and ranking scores are not `My Rating`. Explain any chosen conversion or omission. Verify whether blank/omitted ratings can affect existing remote ratings before claiming preservation.
8. Keep private notes and reasons for adding a book out of `My Review`. Leave review text empty or omit the column according to verified import behavior; this task does not authorize publishing private notes. Do not add a review-sharing feature. Export optional tags only if a safe, documented mapping exists; avoid silently splitting a local multiword tag into multiple shelves.
9. Keep this read-only: no changes to books, readings, placement flags, queue positions, ratings, notes, revision, or sync state. Inspect transaction helpers before using them. Preserve single-connection SQLite constraints and avoid nested queries with open database rows. Prefer no schema changes; any necessary change must follow explicit migration and backup/rollback requirements.

## Acceptance criteria

- Focused synthetic tests cover every local reading status, no blank exclusive shelf, loans/returns, and rereads without fabricated completion.
- ISBN tests cover leading-zero ISBN-10, `X` check digit, ISBN-13, absent/malformed values, and duplicates according to the documented policy.
- Tests cover full-library scope beyond one page and despite filters, deterministic ordering, unavailable metadata, dates, quarter-star rating policy, and private-note exclusion.
- Parse generated output with `encoding/csv` to verify headers, consistent column counts, quotes, commas, newlines, and Unicode. Test attachment headers, authentication, empty output, and relevant failure paths.
- Verify export leaves persisted state and queue order unchanged. Check that existing JSON export, restore, and database backup still work.
- Update user documentation with the upload steps, field/status mappings, exclusions, rating behavior, and Goodreads limitations. Clearly distinguish local CSV verification from an actual Goodreads import; never upload to an account without explicit authorization.
- Complete required checks from `CONTRIBUTING.md`, including race tests, vet, staticcheck, formatting, and module consistency. Include synthetic desktop/mobile screenshots for UI changes.

Finish with the resulting behavior, field and status mapping, exclusions and unresolved compatibility limitations, changed files, and validation results.
