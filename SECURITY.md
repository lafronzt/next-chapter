# Security

This is an early single-reader application, not a multi-tenant service. It uses password login, signed HTTP-only sessions, CSRF tokens, escaped templates, upload limits, and a restrictive content security policy. Login attempts are globally limited to ten per minute for the personal instance. This prevents rapid brute-force attempts but can temporarily block the legitimate reader too.

Deploy behind HTTPS or on a trusted private network. Set `COOKIE_SECURE=1` for HTTPS. Keep secrets outside the image and source control; protect the persistent volume and backups, which contain reading history. Do not run `LOCAL_DEMO=1` on an exposed server. The app makes outbound requests in two optional cases, each to fixed HTTPS hosts with response size and time limits:

- Goodreads RSS polling, to `www.goodreads.com`.
- AI assistance, only when the operator sets `AI_PROVIDER` and an API key and the reader switches it on in Preferences: `api.anthropic.com` or `api.openai.com`, with redirects refused, a 60-second timeout, a 1 MB response limit, and a monthly request cap (`AI_MAX_MONTHLY_REQUESTS`). Each request is started by the reader, after a preview of exactly what will be sent. Requests contain book titles, authors, tags, page counts, series, interest, ratings, and finish years only. Library account and loan details, ISBNs, Goodreads IDs, credentials, and session data are never sent, and review text only when the reader ticks a box for that request.

The AI API key is read from the environment only. It is not stored in the database, exports, or backups, not rendered, and not logged; logs record a failure class, never prompts, answers, or provider error bodies. Keep it in your `.env` or a Kubernetes Secret, and rotate it with the provider if it may have leaked. Model answers are treated as untrusted input: they are decoded strictly, every ID and value is validated, and they can only produce a preview the reader must confirm. Imported book data is sent inside a delimited block marked as untrusted, but that is defence in depth; the validation is what bounds the effect of a prompt injection.

A personal instance has one password and no role separation. Session cookies remain valid until expiry or secret-key rotation; changing only the password does not invalidate existing signed sessions. Rotate `APP_SECRET_KEY` too when revoking access. No formal external security audit has been performed.

Report security problems through the repository's private vulnerability reporting feature if enabled. If unavailable, open an issue asking for a private reporting channel without including exploit details or personal data.
