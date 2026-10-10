# Security

This is an early single-reader application, not a multi-tenant service. It uses password login, signed HTTP-only sessions, CSRF tokens, escaped templates, upload limits, and a restrictive content security policy. Login attempts are globally limited to ten per minute for the personal instance. This prevents rapid brute-force attempts but can temporarily block the legitimate reader too.

Deploy behind HTTPS or on a trusted private network. Set `COOKIE_SECURE=1` for HTTPS. Keep secrets outside the image and source control; protect the persistent volume and backups, which contain reading history. Do not run `LOCAL_DEMO=1` on an exposed server. Only the optional Goodreads RSS connection makes outbound requests, to a fixed HTTPS host with redirects disabled and response size/time limits.

A personal instance has one password and no role separation. Session cookies remain valid until expiry or secret-key rotation; changing only the password does not invalidate existing signed sessions. Rotate `APP_SECRET_KEY` too when revoking access. No formal external security audit has been performed.

Report security problems through the repository's private vulnerability reporting feature if enabled. If unavailable, open an issue asking for a private reporting channel without including exploit details or personal data.
