# Next Chapter

A self-hosted reading queue that makes room for a new book where it belongs—not just at the bottom.

Next Chapter is a **single-reader web app** with two ways to organize your list:

- **Find its place:** suggest where one book belongs, preserving the relative order of every other book.
- **Rethink my queue:** reconsider the entire queue using your interest, library timing, preferred length, tags, and series order.

Every suggestion has a preview and reasons. Pinned books keep their positions. You decide whether to apply a suggestion, and can undo it until another library change occurs. No AI service, API key, or subscription is required; [AI assistance](#ai-assistance-optional) is off unless you connect a hosted model with your own key or a model on your own hardware, and switch it on.

![Next Chapter with example books](docs/preview.png)

## What works in v0.2

- Add and edit books; move them manually; pin positions; mark reading, finished, abandoned, or archived.
- Separate views for Up next, Reading, Finished, To review, Set aside (abandoned), Archived, and All books. Lists show 50 books per page (`per_page` up to 200); queue numbers always count from the top of the whole queue, and a page past the end shows the last page.
- Delete a book, or merge a duplicate into the entry you keep, each after a confirmation preview. Re-imports never bring them back.
- Explainable placement and full-queue reordering, with preview, stale-preview protection, and undo.
- Goodreads CSV imports and optional hourly public-shelf RSS checks for **new additions**.
- A **Reading history** of finished books, with finish dates, your ratings, books per year, and favorite shelves, from your Goodreads read shelf and readings you log yourself.
- Libby timeline CSV imports and a standard CSV format for physical NYPL activity.
- Separate library events for print, ebook, and audiobook; newest known event wins, regardless of import order.
- Repeatable imports, matching by Goodreads ID, ISBN, then normalized title and author.
- Library-only titles enter **unreviewed**, never automatically finished or added to your reading queue. They wait in **To review** (with a count in the sidebar), where one click marks each as Want to read, Already read, or Not interested, or you can sort every visible book at once after a confirmation step.
- Password login, CSRF protection, local SQLite persistence, JSON export and restore, and consistent SQLite backups.
- Docker, Compose, Kubernetes manifests, automated tests, and tagged multi-platform image publishing.

## Integration boundaries

**This app owns the ordered queue. It does not write order, reading status, ratings, reviews, or reading history back to Goodreads.** Goodreads stopped issuing public API keys; this project does not depend on that API or request a Goodreads password.

The optional RSS adapter reads a public shelf. Feeds can be delayed, truncated, unavailable, or changed by Goodreads. Import your full Goodreads CSV first, then enable RSS for new additions. Missing entries in a feed never cause deletions. Feed removals and shelf/status changes are not synchronized, except that a feed entry for a book still waiting in To review moves it to your queue, like a CSV import does. New entries are appended and marked **Needs placement**; suggestions never silently move books. RSS is experimental and requires a real public user ID to validate against your account.

Feed requests go only to `https://www.goodreads.com`. Redirects are followed for at most three hops, and only to that exact host over HTTPS. After a failed scheduled check, the checker waits 1 hour, then 2, 4, and so on, up to 24 hours; a successful check resets the wait. Preferences shows the last result, the number of consecutive failures, the last error time, and the next scheduled check. Settings problems and "a check is already running" each get their own message, separate from feed outages. Server logs give each failure a class such as `timeout`, `status-503`, `redirect`, or `parse`, without the feed URL or user ID.

Libby supports exporting timeline activity but does not provide a direct Goodreads integration. Export column names and activity labels can vary. The parser accepts common variants and rejects unsupported rows with an actionable error; normalize those rows to the provided sample CSV before importing. There is no authenticated Libby connection in v0.2.

**NYPL physical history is not a live connection.** The `examples/nypl.csv` format is an app-defined template, not an official NYPL export. Capture current checkouts and holds from your account, or reconstruct records from receipts you already have. NYPL says physical loan records are normally deleted soon after return, so old history cannot be assumed recoverable. An event disappearing from a snapshot is never inferred to be a return or cancellation. No automatic NYPL account or email scraping is included.

Authoritative references:
- [Goodreads API notice](https://www.goodreads.com/group/show/8095-goodreads-developers)
- [Libby export instructions](https://help.libbyapp.com/en-us/6207.htm)
- [Libby / Goodreads integration limitations](https://help.libbyapp.com/en-us/6211.htm)
- [NYPL privacy policy, circulation records](https://www.nypl.org/help/about-nypl/legal-notices/privacy-policy)

## Run with Docker Compose

Requirements: Docker Engine with Compose.

```sh
cp .env.example .env
# Generate two independent secrets and put them in .env:
openssl rand -hex 32
openssl rand -hex 32
# Set APP_PASSWORD to the first value, APP_SECRET_KEY to the second.
docker compose up --build -d
```

Open [localhost:8000](http://localhost:8000) and sign in with `APP_PASSWORD`. Your data is stored in the named `library` volume. Optional AI assistance, including a local Ollama service under the `ollama` profile, is described in [AI with Docker Compose](#ai-with-docker-compose). Startup fails unless the password is at least 12 characters and the signing key at least 32. `.env` is ignored by Git and excluded from the image.

Compose binds to loopback by default. For access from other devices, place the app behind your HTTPS reverse proxy and set `COOKIE_SECURE=1`, which also sends `Strict-Transport-Security`. Configure the proxy to reach container port 8000 and preserve the host. Keep the app password enabled. Do not publish unauthenticated demo mode.

**Behind a proxy, set `TRUSTED_PROXY_HOPS`.** Failed sign-ins are limited per client address (10 per minute), with a backstop of 100 failures per minute across all clients. By default the client address is the TCP peer and `X-Forwarded-For` is ignored, so it cannot be spoofed. Behind a reverse proxy every request comes from the proxy's address, so all visitors share one limit and one person's typos can lock out everyone for a minute. Set `TRUSTED_PROXY_HOPS` to the number of proxies that append to `X-Forwarded-For` (usually `1`); the app then takes that entry counting from the right, ignoring anything a client prepended. Only set it when every request really passes through that many proxies, or clients can choose their own address.

Sign out on one browser with **Sign out**. **Sign out everywhere** in Preferences ends every session on every device. Changing `APP_PASSWORD` or `APP_SECRET_KEY` also ends every session. When a server error occurs, the page shows a short reference that matches a `request_id=` line in the container log.

The image runs as UID/GID `10001:10001`, with a read-only root filesystem, writable `/tmp`, and persistent `/data`. One Go process serves requests and runs the optional polling goroutine. Use one container/replica per database; SQLite storage should be on a local/block filesystem rather than a shared network filesystem.

### Build or run without Compose

```sh
docker build -t next-chapter:local .
docker run -d --name next-chapter \
  --env-file .env \
  -p 127.0.0.1:8000:8000 \
  -v next-chapter-data:/data \
  --read-only --tmpfs /tmp:mode=1777,size=64m \
  --cap-drop ALL --security-opt no-new-privileges:true \
  next-chapter:local
```

To use an Ollama running on the same computer with `docker run`, add `--add-host=host.docker.internal:host-gateway` and set `AI_BASE_URL=http://host.docker.internal:11434` in `.env` (see [Self-hosted models](#self-hosted-models-ollama-and-openai-compatible-servers)).

The published Go image is `ghcr.io/lafronzt/next-chapter:0.2.0` (also `:latest`), supporting `linux/amd64` and `linux/arm64`. A release tag triggers the included GitHub workflow. Prefer a version tag or digest in production. The final image contains a statically compiled Go executable and CA certificates, with no interpreter or shell. Rebuild with `docker build --pull` to refresh the Go toolchain.

```sh
docker pull ghcr.io/lafronzt/next-chapter:0.2.0
```

### Upgrade from v0.1

Download a database backup, stop the old container, and start v0.2 with the same data volume and environment settings. The Go version preserves books, queue order, preferences, and borrowing history. Sign in again after upgrading.

**The first start makes a one-way change.** Schema migration 2 rewrites every event and loan timestamp into a canonical format and clears pending previews and the last undo. Before it runs, the app writes `next-chapter.sqlite3.pre-v2.bak` next to the database (see [Backup and restore](#backup-and-restore)). Never run the v0.1 Python image against the migrated file. To roll back, restore the `.bak` as described below and run v0.1 against that.

## Kubernetes

Use a cluster with a default StorageClass and a local/block-backed persistent volume. The deployment intentionally uses **one replica** and **Recreate** updates to avoid overlapping SQLite writers/pollers; upgrades have brief downtime.

1. Build and push your image, or publish a release tag with the included GitHub workflow.
2. Replace the image in `deploy/kubernetes/app.yaml` with your registry image and version.
3. Create the secret from the same `.env` file, apply the manifest, and port-forward:

```sh
kubectl create secret generic next-chapter-auth --from-env-file=.env
kubectl apply -f deploy/kubernetes/app.yaml
kubectl port-forward service/next-chapter 8000:80
```

For optional AI assistance, see [AI on Kubernetes](#ai-on-kubernetes); `deploy/kubernetes/ollama.yaml` runs a private Ollama server in the cluster if you want local models.

The PVC defaults to 1 GiB. The manifest includes resource limits, readiness/liveness checks, a non-root user, dropped capabilities, and a writable temporary directory. `/healthz` is unauthenticated and returns only a health status.

For an HTTPS Ingress, configure your preferred controller and certificate, then set `COOKIE_SECURE` to `"1"` in the deployment. The example leaves it at `"0"` for HTTP loopback port-forwarding. Avoid setting conflicting values in both the secret and deployment; the explicit deployment value wins.

Kubernetes manifests are supplied as deployment templates. You must choose your storage class, ingress, TLS certificate, namespace, image pull access, and backup destination. No cluster is modified by this repository.

## Ranking behavior

Each queued book starts with `interest × 10` points (10–50). Additional points:

| Signal | Points |
| --- | ---: |
| Ready for pickup | 25 |
| Already borrowed | 15 |
| Available | 5 |
| Loan / pickup deadline within 7 days, including overdue | +20 |
| Exact mood tag match, case-insensitive | 15 |
| Preferred length match (≤250 or ≥400 pages) | 10 |

Only the strongest library signal is used per book; multiple formats do not stack. Library points are multiplied by the selected timing weight (0, 0.5, 1, or 2). All dates used for ranking use UTC. Holds with no pickup date get no timing boost because a request does not imply availability.

Full-queue suggestions sort by score while respecting pins and earlier queued books in the same series. Equal scores preserve existing order. Series metadata is supplied by the reader; Goodreads export/RSS does not reliably provide it. Insertion finds the earliest lower-scoring book, then the closest feasible slot that preserves the other books' order, pin slots, and the candidate's series dependencies. Impossible constraints produce a clear error instead of silently moving pins. Manual moves are intentional overrides of ranking; pin crossings must be unpinned first.

Preferences affect the next suggestion; saving them does not move books. Reading/finished/archived/unreviewed books are outside the queue. Returning a book never changes its reading status.

## AI assistance (optional)

Next Chapter works fully without AI. If you want suggestions from a language model, you can connect:

- **A hosted model** from Anthropic (Claude) or OpenAI with your own API key. You pay the provider directly.
- **A model on your own hardware** with [Ollama](https://ollama.com), so nothing leaves your machine or network.
- **Any other model** behind an OpenAI-compatible server, such as LM Studio, vLLM, llama.cpp's server, LocalAI, or a gateway you run.

There is no hosted proxy and no paid service from this project.

It is **off by default** and needs two steps:

1. **The operator** sets a provider and model in the environment (plus a key for hosted providers) and restarts the app:

   | Variable | Meaning |
   | --- | --- |
   | `AI_PROVIDER` | `off` (default), `anthropic`, `openai`, `ollama`, or `custom` |
   | `AI_MODEL` | The model name, required. Hosted suggestions checked on 2026-10-13: `claude-opus-5-5` for Anthropic, `gpt-6-astra` for OpenAI; a cheaper model costs less per request. For `ollama`, any model you have pulled, such as `llama3.1:8b` or `qwen2.5:7b`. For `custom`, the model name your server lists. |
   | `AI_API_KEY` | Required for `anthropic` and `openai` (`ANTHROPIC_API_KEY` or `OPENAI_API_KEY` also work outside Compose). Optional for `ollama` and `custom`, which send it as a bearer token when set, for a server behind an authenticating proxy. |
   | `AI_BASE_URL` | For `ollama`, the server address, default `http://127.0.0.1:11434`. For `custom`, required: the OpenAI-compatible API root, usually ending in `/v1`, such as `http://127.0.0.1:1234/v1`. Plain `http://` is accepted only for servers on this machine or a private network (see below); anything else needs `https://`. For `anthropic` and `openai` it exists only for tests and accepts nothing but `https://127.0.0.1:<port>`. |
   | `AI_TIMEOUT_SECONDS` | How long to wait for one answer, 5 to 600, default `60`. Raise it for slow local models; the request's page waits that long too. |
   | `AI_MAX_MONTHLY_REQUESTS` | Requests allowed per calendar month (UTC), default `100`. Local models cost nothing per request, so raise it freely there. |

   The app refuses to start if the provider is missing something it needs: a model, a key for hosted providers, a base URL for `custom`, or HTTPS for a server outside your network.

2. **The reader** turns on **AI assistance** in Preferences. Preferences shows the provider, model, and (for self-hosted servers) the server address and whether it is on your network, this month's requests and tokens, a plain-language summary of what is shared, and a **Test connection** button that sends a fixed prompt with no library data.

**What is sent, and when.** Nothing is sent in the background, on a schedule, during a Goodreads check, or when a page loads. Each AI action first shows a **What gets shared** list of the exact rows and fields it will send, and sends only after you confirm. Requests carry only what the task needs: titles, authors, tags, page counts, series, interest, your ratings, and finish years. They never carry library-card or loan account details, your password or session, ISBNs, Goodreads IDs, or review text (unless you keep reviews and tick a box for that one request). With a hosted provider, its data terms apply; OpenAI requests ask it not to store the response (`store: false`). With a self-hosted server, whoever runs that server can see what is sent.

**Safety rails.**

- Hosted requests go only to `https://api.anthropic.com/v1/messages` or `https://api.openai.com/v1/responses`. Self-hosted requests go only to the configured `AI_BASE_URL`, at `/api/chat` for Ollama or `/chat/completions` under the API root for `custom`. Plain `http://` is accepted only when the host is `localhost`, a loopback, private (10.x, 172.16-31.x, 192.168.x, fc00::/7), or link-local IP address, a single-word name such as a Compose service (`ollama`), or a name ending in `.local`, `.lan`, `.internal`, `.home.arpa`, or `.svc`. A URL with a username, password, query, or fragment is refused.
- Every request has the configured timeout, a 1 MB response limit, no redirects, and `User-Agent: NextChapter/<version>`. Only `net/http` is used; there is no vendor SDK.
- The key is read from the environment only. It is never stored in the database, exports, or backups, never shown on a page, and never logged. Logs record only a failure class such as `status-429` or `timeout`, never prompts, answers, or provider error bodies.
- One AI request runs at a time, and never while holding the database: inputs are read first, the provider is called, then a short write saves the result. Every attempt, including failed ones, counts toward the monthly limit; when it is used up, AI actions say so until the next month.
- Model output is untrusted. The app asks for JSON matching a schema, then decodes it strictly (unknown fields are an error) and checks every value: IDs must be ones it sent, with no repeats, and numbers must be in range. Anything else is discarded with a plain message. An answer can only propose; it never writes to your library directly.
- Book data comes from imported files, so it is sent inside a delimited data block, and the model is told to treat it as data, not instructions. The schema check above is the real defence.

To turn AI off again, switch it off in Preferences, or set `AI_PROVIDER=off` (or remove it) and restart.

### Self-hosted models: Ollama and OpenAI-compatible servers

Requests to Ollama use its native chat API with a JSON Schema in `format`; requests to a `custom` server use Chat Completions with `response_format` set to a strict JSON schema. Both repeat the schema in the prompt and use temperature 0, which helps smaller models follow it. Self-hosted servers also get length limits in the schema (for example, at most three reasons of at most 240 characters), which their grammar-based decoding enforces while the model writes; without them, a small model can repeat itself until it reaches the token limit. Hosted APIs reject those keywords, so they get the plain schema and the app checks lengths afterwards. The server must support JSON-schema structured output: Ollama does, and so do current LM Studio, vLLM, and llama.cpp servers. A model that ignores the schema produces answers the app discards with a message, never a change. Instruction-tuned models of about 7B parameters or more follow it far more reliably than tiny ones. Small local models give simpler suggestions than large hosted ones.

Ollama is also OpenAI-compatible, so `AI_PROVIDER=custom` with `AI_BASE_URL=http://127.0.0.1:11434/v1` works too; `ollama` is simpler and needs no path.

Where Ollama runs decides `AI_BASE_URL`:

| Ollama runs… | `AI_BASE_URL` |
| --- | --- |
| On the same computer, app run directly | `http://127.0.0.1:11434` (the default) |
| In the Compose `ollama` profile (below) | `http://ollama:11434` |
| On the computer that runs Docker, outside Docker | `http://host.docker.internal:11434`. Ollama listens only on 127.0.0.1 by default; start it with `OLLAMA_HOST=0.0.0.0` so containers can reach it, and keep port 11434 closed to the internet. |
| On another machine on your network | `http://192.168.1.20:11434` (its address), with `OLLAMA_HOST=0.0.0.0` on that machine |
| In Kubernetes from `deploy/kubernetes/ollama.yaml` | `http://ollama:11434` |
| Anywhere reached over the internet | `https://…` only, ideally behind an authenticating proxy, with `AI_API_KEY` as its bearer token |

Pull the model before switching AI on (`ollama pull llama3.1:8b`); otherwise the test reports that the server does not know the model. A first request after the server starts may be slow while the model loads, so allow for it in `AI_TIMEOUT_SECONDS`.

### AI with Docker Compose

`compose.yaml` passes the `AI_*` variables from `.env` to the app (as `AI_API_KEY`; Compose does not pass the vendor-specific key names). It also defines an optional `ollama` service under a profile, so it only runs when asked:

```sh
# in .env: AI_PROVIDER=ollama, AI_BASE_URL=http://ollama:11434, AI_MODEL=llama3.1:8b
docker compose --profile ollama up -d
docker compose exec ollama ollama pull llama3.1:8b
```

Models are kept in the `ollama` volume. The Ollama port is not published, so only the app can reach it; for GPU acceleration, add your GPU settings to the service as Ollama's Docker instructions describe. For a hosted provider, set `AI_PROVIDER`, `AI_MODEL`, and `AI_API_KEY` in `.env` and run `docker compose up -d` as usual.

### AI on Kubernetes

Keep any key in a Secret, separate from the manifest. The simplest way is to add the `AI_*` lines to the same `.env` you create `next-chapter-auth` from. To keep the AI key in its own Secret instead:

```sh
kubectl create secret generic next-chapter-ai --from-literal=AI_API_KEY='…'
```

Then uncomment the `AI_*` entries you need in `deploy/kubernetes/app.yaml`. Rotating the key means updating the Secret and restarting the pod.

For models in your cluster, `deploy/kubernetes/ollama.yaml` adds a single Ollama Deployment with a 20 GiB volume for models and a cluster-internal Service. Size its memory limit and volume for your models, add a GPU resource limit if you have a device plugin, and pull a model once:

```sh
kubectl apply -f deploy/kubernetes/ollama.yaml
kubectl exec deploy/ollama -- ollama pull llama3.1:8b
```

Then set `AI_PROVIDER=ollama`, `AI_BASE_URL=http://ollama:11434`, `AI_MODEL`, and a longer `AI_TIMEOUT_SECONDS` for the app. Restrict who can reach the Ollama Service with a NetworkPolicy if your cluster is shared.

## Imports and data ownership

CSV files must be UTF-8, ≤2 MB, and ≤10,000 rows. A malformed row blocks the entire import, with row numbers shown. Import previews expire after an hour. A library change invalidates prior previews, preventing an old browser tab from overwriting newer work.

- `Title` and `Author` are required for every record.
- Goodreads fields: `Book Id`, `ISBN13` or `ISBN`, `Number of Pages`, `Exclusive Shelf`, `Bookshelves`, `My Rating`, `Date Read`, `My Review`, `Average Rating`, and `Original Publication Year` (or `Year Published`). The bundled sample has every column of a Goodreads export. An unreadable `Date Read` is a row error, like any other bad date; `Date Read` may be blank.
- **Ratings seed interest for new books only.** A `My Rating` of 1–5 (or the RSS feed's `user_rating`) becomes the starting interest of a book the import adds. `0` or blank means unrated and starts at 3. Any other value, such as `7` or `abc`, is ignored rather than failing the import; the book starts at 3 and the preview says how many ratings were ignored. A book already in your library keeps the interest you set, whatever its Goodreads rating. `Average Rating` is never used: it is other readers' opinion, not yours. Rated books on the `read` shelf are stored as usual; interest only matters if they return to the queue. The preview shows how many new books take their rating and how many existing books keep theirs.
- Library fields: `Timestamp` (or `Date`), `Activity` (or `Status`), optional `Format`, `ISBN`, and `Due Date`.
- Prefer ISO 8601 timestamps with time zones and `YYYY-MM-DD` due dates. Date-only and timezone-free inputs are treated as UTC. If several different events share the same timestamp and format, the first one imported remains the current status; supply more precise timestamps or log the current status manually to resolve ambiguity.
- Supported activities include Borrowed, Loan, Checked out, Renewed, Hold, Placed on hold, Requested, Ready, Ready for pickup, Returned, Cancelled/Canceled, and Available.
- Use consistent format names (`print`, `ebook`, `audiobook`, or `digital`) across imports. Format names distinguish parallel loans.
- Initial imports use file row order for new books. Existing books keep local reading status, interest, tags, and queue order on subsequent imports; missing identifier/page metadata can be filled in.
- One exception: a book that arrived through a library import and is still **unreviewed** takes its status from the first Goodreads shelf that mentions it (CSV or RSS). `to-read` appends it to the end of your queue marked **Needs placement**; `read`, `currently-reading`, and other shelves become finished, reading, or archived. The import preview says how many books will move. Books you have already sorted are never changed by an import, and library activity never changes reading status. Import order therefore no longer matters.
- Matching by normalized title + author is conservative about spelling and subtitles. Different titles without a shared identifier may require manual cleanup. Edition-level history is represented through source/format; books are grouped into one reading-list entry when their identifiers/title match.

### Reading history

Books on your Goodreads `read` shelf become **Reading history**: one entry per finished reading, with the finish date (`Date Read`) and your rating (`My Rating`). The Reading history page lists them newest first, with books per year, your average rating, average pages, and the shelves and tags you return to. Each book's page shows its readings, and lets you log one by hand or correct a date or rating.

- History is its own record. It never changes a book's reading status, interest, pin, or queue position, and library loans never count as reading.
- Re-importing or re-checking the same data changes nothing. A new `Date Read` for a book you finished before is recorded as a re-read. If Goodreads later fills in a date that was blank, the reading takes that date. A changed Goodreads rating updates the reading.
- **Your edits win.** After you change a reading's date or rating in the app, Goodreads checks leave it alone. You can delete readings you logged yourself. A reading from Goodreads can be edited but not deleted, because the next import or check would add it again (deleting the book removes its readings).
- The hourly Goodreads check also reads a second shelf, **Read shelf** in Preferences (default `read`; leave it blank to skip). It uses the same feed rules and limits as the main shelf, and Preferences shows when it was last checked and how many readings it logged. A finished book that is not in your library yet is added as Finished. RSS feeds hold only the most recent entries, so **import your Goodreads CSV once for the full history**; the feed keeps it current. Unreadable feed dates are skipped and counted rather than failing the check.
- **Reviews stay out by default.** Only the length of your Goodreads review (`My Review`) is stored. To keep the text in this library, turn on **Keep my Goodreads review text** in Preferences; turning it off again deletes the stored text. Without it, review text is not kept even in a pending import preview.
- Not available from Goodreads, and so not here: reading progress and start dates, dates of earlier re-reads (Goodreads keeps only the latest `Date Read`; `Read Count` is not a list of dates), private notes, and anything on a private profile. This app does not scrape Goodreads, ask for your Goodreads password or cookies, or use unofficial APIs.

### Deleting and merging books

Both actions start from a book's edit page under **Tidy up**. Nothing is merged or deleted automatically.

- **Delete** opens a confirmation page that shows how many library events and current loans will go with the book. The remaining queue keeps its order.
- **Merge into another book…** lets you search for the entry to keep and preview the result before anything changes. The kept book keeps its title, author, reading status, interest, pin, and queue position. Its blank Goodreads ID, ISBN, pages, series, series number, and tags are filled from the merged book. Library events move over with duplicates dropped, and current loans are recalculated using the newest-wins rule. Books with different Goodreads IDs cannot be merged. If the two ISBNs differ, the preview says so and the kept book's ISBN wins.
- Both actions leave an alias with the removed book's title/author, Goodreads ID, and ISBN. When an import matches an alias, a merged book's rows update the kept book, and a deleted book's rows are skipped. The import preview, the import notice, and the Goodreads status line all show how many rows were skipped. Adding a deleted book again by hand removes its alias, so your explicit choice wins.
- Deleting a book removes its readings. Merging moves them to the kept book, dropping any reading on a date the kept book already has.
- Aliases are part of the database backup and appear under `aliases` in the JSON export. Neither action can be undone in the app; download a backup first if you might want the history.

## Backup and restore

In Preferences, download a **database backup** for full recovery, or JSON for portable readable data that can also be restored (see below). Backups use SQLite’s `VACUUM INTO` snapshot operation, not a potentially incomplete copy of a live WAL file. Pending previews, the undo snapshot, login-failure counters, and the session key are omitted, so a restored copy never accepts sign-in cookies issued by the original; everyone signs in again after a restore. The backup still contains personal reading history and your public Goodreads connection settings.

To restore:

1. Stop the app and preserve your current volume as a fallback.
2. Use a new empty data volume/directory, put the downloaded backup there as `next-chapter.sqlite3`, and set ownership to `10001:10001`.
3. Point the app at that directory/volume and restart with your existing secret configuration. Keep `APP_SECRET_KEY` stable across normal restarts; changing it signs out existing sessions.

Do not restore over a running database or mix an older database with newer `-wal`/`-shm` files.

### Restore from the JSON export

The JSON export can also be loaded back, for example to move to a new instance when only the JSON was kept. On **Import books**, choose **Restore a Next Chapter JSON export** (up to 10 MB). Like an import, it validates the whole file, shows a preview with counts of books, library events, current loans, and aliases, and changes nothing until you confirm; a library change in between invalidates the preview.

- Restore only works on an **empty library** (no books). Merging an export into an existing library is not supported.
- Restored: books with their status, interest, pins, **Needs placement**, queue positions and order, series, tags, pages, identifiers, added date, and source; reading history (review text only if the restored preferences keep reviews); library events with their fingerprints, so re-importing the same CSV adds no duplicates; current loans, rebuilt from the events with the same newest-wins rule as imports; merge and deletion aliases; ranking preferences; and Goodreads settings.
- Goodreads polling is always restored **off**. The preview and the confirmation say so; turn it on in Preferences when ready.
- Not restored: previews, undo, sign-in state, and the session key. Books and events get new ID numbers.
- Validation is strict and uses the same limits as the forms and CSV imports. Unknown fields, malformed JSON, out-of-range values, duplicate titles or Goodreads IDs, and exports from a newer schema version than this build supports are refused, and nothing is written. The export's `schema_version` is the database schema version it came from; this build restores versions 1 to 6. An export from before version 6 has no reading history, and its Goodreads connection starts following the `read` shelf.
- The file is limited to 10 MB, 10,000 books, and 100,000 events. A 10,000-book export is typically about 6 MB; a library too large for JSON restore can still use the database backup.

### Schema upgrades and rollback

Preferences shows the database schema version. On startup, the app applies any pending schema migrations in order. Each one runs in its own transaction, so a failed step leaves the database at its previous version. Before changing an existing database, the app writes a `VACUUM INTO` snapshot next to it, named after the first pending version, for example `next-chapter.sqlite3.pre-v3.bak` (mode `0600`). An existing backup is never overwritten; a second attempt writes `.pre-v3.1.bak`. Startup fails if the snapshot cannot be written. New, empty databases are not backed up.

These `.bak` files stay on the data volume until you remove them, and each one is about as large as your database. Leave enough free space for one extra copy before upgrading, and delete old `.bak` files once you are happy with the new version.

The app refuses to start if the database has a newer schema version than the running image understands, so an older image never writes to a newer schema. To roll back an upgrade:

1. Stop the app.
2. Replace `next-chapter.sqlite3` with the matching `.bak` file, for example `cp next-chapter.sqlite3.pre-v3.bak next-chapter.sqlite3`, and delete any stray `next-chapter.sqlite3-wal` and `next-chapter.sqlite3-shm` files. Keep ownership `10001:10001`.
3. Start the previous image.

Changes made after the upgrade are not in the `.bak`. Export JSON first if you need a record of them.

Schema migration 6 adds the `reading_log` table, the `books.published_year` and `books.avg_rating` columns, and a `read_shelf` of `read` to a saved Goodreads connection. Roll back with `next-chapter.sqlite3.pre-v6.bak` as above. To keep changes made since the upgrade instead, run an older image after `DROP TABLE reading_log; ALTER TABLE books DROP COLUMN published_year; ALTER TABLE books DROP COLUMN avg_rating; UPDATE meta SET value='5' WHERE key='schema_version';`; this discards the reading history, so export JSON first.

Schema migration 5 only adds indexes for paging and imports; no data changes. Rolling back from it works as above with `next-chapter.sqlite3.pre-v5.bak`. Alternatively, an older image runs against the same file after `DROP INDEX books_status_position; DROP INDEX books_position; DROP INDEX books_isbn; DROP INDEX events_book; DROP INDEX events_at;` and `UPDATE meta SET value='4' WHERE key='schema_version';`, which keeps changes made since the upgrade.

## Local development

Go 1.27 or later; no frontend build step or C compiler is required:

```sh
go test -race ./...
go vet ./...
go build -trimpath -o ./bin/next-chapter ./cmd/next-chapter
LOCAL_DEMO=1 START_POLLER=0 ./bin/next-chapter
```

CI also requires `gofmt`, `staticcheck`, `govulncheck`, a tidy `go.mod`, and a container smoke test; [CONTRIBUTING.md](CONTRIBUTING.md#required-checks) lists the exact commands.

Open [localhost:8000](http://localhost:8000). Demo mode has no login and requires a loopback `LISTEN_ADDR` (default `127.0.0.1:8000`). The empty-library page lets you load example books. Production starts with an empty library and never seeds examples.

To try AI assistance locally without an API key, run Ollama, pull a model, and start the app with it:

```sh
ollama pull llama3.1:8b
AI_PROVIDER=ollama AI_MODEL=llama3.1:8b AI_TIMEOUT_SECONDS=300 LOCAL_DEMO=1 START_POLLER=0 ./bin/next-chapter
```

Then switch AI on in Preferences. Tests never contact a real provider; they use `httptest` servers.

For production, set `APP_PASSWORD` and `APP_SECRET_KEY` and omit `LOCAL_DEMO`. `DATABASE_PATH` defaults to `data/next-chapter.sqlite3`; `LISTEN_ADDR` defaults to `:8000`. `START_POLLER=0` disables the background checker. `TRUSTED_PROXY_HOPS` (default `0`) is described under Docker Compose. The executable includes `version` and `healthcheck` commands.

The application uses Go’s HTTP server, escaped HTML templates, plain CSS, and a pure-Go SQLite driver. Templates, assets, and CSV examples are embedded in the executable. No font, analytics, or cover-image service receives reading data. The app makes outbound requests only for opt-in Goodreads RSS and, if the operator configures a provider and the reader switches it on, for [AI assistance](#ai-assistance-optional) that the reader requests and confirms one request at a time. With Ollama or another self-hosted model, those requests can stay on your own machine or network.

## Project layout

- `cmd/next-chapter/`: executable, health check, graceful shutdown.
- `internal/app/model.go`, `propose.go`: data types and deterministic ranking with its constraints.
- `internal/app/imports.go`: CSV/RSS parsing and validation.
- `internal/app/migrations.go`, `migrate.go`: ordered schema migrations and the runner that applies them after a pre-migration backup.
- `internal/app/store.go`, `books.go`, `listing.go`: transactions, book and event queries, paged lists.
- `internal/app/records.go`: matching imported records and event reconciliation.
- `internal/app/plans.go`: compact storage for previews.
- `internal/app/merge.go`: deleting and merging books, aliases that stop re-imports from recreating them.
- `internal/app/feed.go`, `sync.go`: fixed-host RSS fetching, optional hourly polling with backoff.
- `internal/app/ai.go`, `handlers_ai.go`: the optional AI provider layer (Anthropic, OpenAI, Ollama, and OpenAI-compatible servers over `net/http`), its monthly limit, strict answer parsing, and the Preferences switch.
- `internal/app/server.go`: configuration and the routing table; `middleware.go`, `session.go`, `render.go`, `forms.go` hold the request pipeline, and `handlers_*.go` the handlers by area.
- `internal/app/version.go`: the release version, set at build time with `-ldflags "-X github.com/lafronzt/next-chapter/internal/app.Version=…"`.
- `internal/app/web/`: embedded templates, assets, and import examples.
- `internal/app/*_test.go`: ranking, import integrity, migrations, delete/merge, feed and poller, authentication, and web-flow tests.
- `deploy/kubernetes/`: single-replica persistent deployment, and an optional Ollama server for local AI models.

## Next steps

- Validate import adapters against anonymized real exports from more Libby locales.
- Add editable library-event corrections, and a way to review or clear aliases left by merges and deletions.
- Explore opt-in NYPL receipt/email ingestion or an authorized account connector.
- Add optional Goodreads writeback only if a supported integration becomes available.
- Consider multiple reader accounts and PostgreSQL after the personal workflow is proven.

Contributions are welcome under the [MIT license](LICENSE). See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).
