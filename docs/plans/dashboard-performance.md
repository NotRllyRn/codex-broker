# Dashboard performance

Keep Go/Pongo2 templates and plain JavaScript. The initial authenticated page
renders a small shell; a separate endpoint supplies the existing dashboard HTML.
No framework migration or background snapshot service is needed.

1. Cache dashboard HTML, usage summaries, and proxy statistics in sessionStorage,
   scoped to the authenticated session and application asset version. Reuse fresh
   entries; revalidate expired entries with ETags. Bound both browser and server
   caches. Storage failures fall back to ordinary authenticated requests.
2. Cache successful server responses briefly (dashboard/proxy: 5 seconds; usage:
   30 seconds), authenticate before every cache lookup, and return 304 when the
   representation is unchanged. Explicit refresh bypasses both freshness timers.
   Cache keys include session, CSRF cookie, URL, and streamer preference.
3. Render all account cards once and filter locally, preserving query parameters
   and browser back/forward navigation. Streamer mode swaps the corresponding
   cached view without navigation. Dispose timers, requests, and event streams
   before initializing a replacement view.
4. Select usage report dates in SQL; read the history start separately without
   loading old payloads. Keep the existing single database connection for now.

Freshness is deliberately bounded, not realtime: normal dashboard/proxy reads
may lag five seconds and usage thirty seconds. State events invalidate browser
entries; Refresh view forces fresh reads. No stale fallback after authentication
failure. A new login has a different cache namespace; logout clears tab caches.
Streamer snapshots must redact embedded profile labels as well as visible HTML.

Validate authentication and session isolation, 304 responses, forced refresh,
expiry, bounded caches, streamer privacy, local filters, and repeated reloads.
Use isolated synthetic accounts/history for scale checks, never production data.
