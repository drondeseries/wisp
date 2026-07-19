# wisp roadmap / deferred ideas

Running notes on things we've decided to build but deliberately sequenced later,
so nothing gets lost. Not a commitment order — a parking lot with context.

## Bundled web frontend (parked — revisit when the engine settles)

**Idea:** now that wisp is a standalone engine (not a sidecar), give it its own
first-party web UI for managing items and exposing knobs — instead of only the
HTTP API + a media server's request UI.

**Proposed shape (lean + fast, keeping the single-binary story):**
- Embed static assets in the Go binary via `embed.FS` — no second container, no
  separate frontend deploy. Served on the same `:8080`, under `/ui` (the `/api`
  surface stays exactly as-is; the UI is just a client of it).
- A small, fast SPA (Preact/Svelte/vanilla — bias to tiny bundle, instant load).
- Pages: **Library** (pins: view/search/delete, per-quality), **Monitors /
  Schedule** (the watchlist, states, `next_release`/`next_check`, force-refresh,
  tier-backoff status), **Settings** (the knobs: concurrency, notify targets,
  release gating, log level), **Status** (mount health, metrics, version).
- Auth: the API is currently unauthenticated (trusted-network). A UI probably
  wants at least a simple token/gate before it's exposed — design that in.

**Why later:** the API and monitor internals it would render (schedule
durability, tier backoff) are still in flux. Build the UI on a settled API so it
isn't churned. Good candidate once v1.2 series-efficiency + `/api/schedule`
durability land.

## Deleted-media reconcile (the Decline fix)

Silo fires no webhook on media deletion, and the request_router contract has no
cancel RPC — so a Declined/Cleared title can't tell wisp to unpin. Build a
low-frequency loop that polls Silo for removed/cleared media and unpins the
matching pins + notifies the media server. Needs a Silo admin API key and a
blast-radius-safe design (affirmative-removal signal only, per-pass cap, positive
id match). Directly fixes "I can't get rid of a request."

## Never-resolving-tier backoff (in progress)

Back off hard on repeated `errNoQualityMatch` (e.g. 4K for a network-TV show) so
one unsatisfiable tier doesn't keep a whole title on the tight retry cadence.
Capped, never a hard give-up. Feeds `/api/schedule` surfacing.

## /api/schedule durability + self-repair

Expire/clean completed/failed/stale monitors; surface tier states ("2160p:
unavailable, next retry in 7d"); reconcile the monitor list against actual pins.
Make the schedule view durable and self-healing rather than an ever-growing log.

## Season-pack enumeration (blocked on AIOStreams)

True "one scrape → whole season" needs AIOStreams to expose a pack's episode list
(filed: Viren070/AIOStreams#1118). Until then, parallelization + cache-warming is
the mitigation. Revisit if the upstream capability lands.
