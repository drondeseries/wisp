# wisp

![CI](https://github.com/dreulavelle/wisp/actions/workflows/ci.yml/badge.svg)

A standalone request-to-playback engine for [AIOStreams](https://github.com/Viren070/AIOStreams).

wisp turns the streams AIOStreams selects into ordinary-looking media files. It
never downloads anything — each virtual file's bytes are range-proxied from the
resolved stream on demand. Point any media server (Silo, Plex, Jellyfin, Emby)
at the mount and it scans, probes, and plays them like local files.

Point [Overseerr/Jellyseerr](https://github.com/seerr-team/seerr) at wisp and the
whole stack is just **Seerr + wisp + AIOStreams** — no `*arr` apps, no download
client, no media-server plugin. Seerr owns requests, approvals, and users; wisp
owns release scheduling, stream resolution, and the virtual library.

> **AIOStreams is required.** wisp talks to AIOStreams' Search API directly (not
> the generic Stremio addon protocol). That's the point of leverage, not a limit:
> AIOStreams is the aggregator, so whatever Stremio scrapers, debrid services, or
> usenet sources you configure *there* (Torrentio, Comet, MediaFusion, Easynews,
> …) all flow through to wisp.

## How it works

- **Request.** A user approves a request in Seerr; its webhook hits wisp. wisp
  checks eligibility (home-media release for movies, aired episodes for series),
  pins what's available now, and **monitors** the rest.
- **Monitor.** wisp keeps a persistent watchlist and pins unreleased movies and
  newly-aired episodes as they land — waking near the next airstamp, not polling
  blindly. (Or feed it directly via the [API](#api); Seerr is optional.)
- **Mount.** wisp self-mounts with embedded rclone; pins appear under four
  category roots — `movies/`, `shows/`, `anime_movies/`, `anime_shows/` — that
  any media server scans (see [Library layout](#library-layout)).
- **Play.** On open, wisp range-proxies bytes from the pinned stream, which
  re-unlocks the debrid link on every request. If a link has died, wisp
  re-resolves through AIOStreams and playback self-heals.

Because the files carry real bytes, the media server reads real metadata
(codecs, duration, subtitles) and owns playback end to end — direct play,
transcode, and seeking all work.

## Library layout

wisp presents four category roots under the mount, and always shows all four —
even when empty, so a media server can validate every library path from a fresh
install:

```
/mnt/wisp/
├── movies/          # non-anime movies
├── shows/           # non-anime series
├── anime_movies/    # anime movies
└── anime_shows/     # anime series
```

Point Silo at all four (one library per root). A title's category is decided
**once** and is then permanent — it is stored on the title and on every pin, and
never re-derived (the root is part of each file's path, so moving it would orphan
the on-disk files). Re-categorizing an existing title is out of scope for now; a
conflicting later request keeps the first category and logs a warning.

**How anime is decided (in order):**

- If the title already has pins, their existing root is inherited (first writer
  wins — a later flag never splits a title across roots).
- An explicit `is_anime` flag on the request wins next (e.g. from a Silo
  request); this is resolved immediately, so `/api/add` never blocks.
- Otherwise, when the flag is omitted, the category is resolved by the scheduler
  on its first pass — *before anything is pinned* — using a small, conservative
  heuristic over the Cinemeta metadata wisp already fetches: it requires the
  **Animation** genre **and** a Japanese original language/country. Western
  animation and any title without a Japanese signal stay non-anime. Deferring
  this off the intake path keeps `/api/add` fast.
- If no signal is available, the title defaults to non-anime.

## Quick start

wisp embeds rclone and self-mounts the library — one container, no separate
rclone process. Copy the env template and fill in your AIOStreams URL + password:

```sh
cp .env.example .env      # then edit .env
```

```yaml
services:
  wisp:
    image: ghcr.io/dreulavelle/wisp:latest
    container_name: wisp
    env_file: .env                          # all WISP_* settings (see .env.example)
    volumes:
      - ./data:/data                        # persist the pin + monitor database
      - /mnt/wisp:/mnt/wisp:rshared         # share the mount out to the host
    devices:
      - /dev/fuse
    cap_add:
      - SYS_ADMIN
    security_opt:
      - apparmor:unconfined
    ports:
      - "8080:8080"
```

Prepare the host mountpoint once, so the in-container FUSE mount propagates out
to the host (and into your media-server container):

```sh
mkdir -p /mnt/wisp
mount --bind /mnt/wisp /mnt/wisp && mount --make-rshared /mnt/wisp
```

Then point your media server (Silo, Plex, Jellyfin, Emby — however you already
run it) at `/mnt/wisp`. If it runs in its own container, give it the mount with
`:ro,rslave` so it sees wisp's FUSE mount and re-sees it after a wisp restart:

```yaml
    volumes:
      - /mnt/wisp:/mnt/wisp:ro,rslave
```

Feed wisp a title (see [API](#api)) and it appears as a real file ready to scan
and play. See [Deployment](docs/Deployment.md) for propagation details and making
the host-share survive reboots.

`rm` on a mounted media file unpins it from wisp; creating, editing, and
renaming mounted files stay read-only by design.

> **HTTP-only alternative.** Leave `WISP_MOUNT_PATH` unset (and drop `devices`,
> `cap_add`, `security_opt`, and the `:rshared` volume) to serve the library
> over HTTP on `:8080` and mount it yourself with rclone.

## Notifying your media server

When wisp pins, renames, or deletes a file it tells your media server to rescan,
so new content appears instantly instead of on the next periodic scan. Configure
any combination of targets — wisp notifies all of them. Every target resolves
paths against `WISP_MOUNT_PATH` (default `/mnt/wisp`), the path your media server
sees on disk. Delivery is fire-and-forget on a background goroutine: a slow or
unreachable server never blocks or fails a pin or delete, and each target logs
its own failures.

All targets fire on three events:

- **Import** — a newly pinned movie or episode.
- **Rename** — a re-resolve changed the virtual filename (old path removed, new
  path scanned).
- **File Delete** — a pin removed via the API or a mounted `rm`.

### Silo (recommended)

Create one webhook source in **Silo → Autoscan → Sources**, copy its webhook
URL, and add it to wisp:

```yaml
environment:
  WISP_NOTIFY_ARR_WEBHOOK_URL: https://silo.example.com/api/v1/autoscan/webhooks/<secret>
```

> **This is not Radarr/Sonarr integration** — wisp needs no Radarr or Sonarr, and
> talks to neither. The `ARR` in the variable name refers only to the *wire
> format*: wisp emits the JSON shape that a Sonarr/Radarr webhook would send,
> because that is the payload media-server autoscan intakes (Silo's included)
> accept natively. wisp is the sole source of these notifications.

`WISP_SILO_WEBHOOK_URL` is still accepted as a deprecated alias (if both are set,
the canonical `WISP_NOTIFY_ARR_WEBHOOK_URL` wins). Because it is that standard
webhook format, it can also drive an actual Sonarr/Radarr instance if you happen
to run one — but that is entirely optional and unrelated to wisp's operation.

### Jellyfin / Emby

wisp posts a `Library/Media/Updated` rescan hint using an admin API key. Silo's
Jellyfin-compatible endpoint works here too.

```yaml
environment:
  WISP_NOTIFY_JELLYFIN_URL: http://jellyfin:8096
  WISP_NOTIFY_JELLYFIN_API_KEY: <api-key>
  # Emby is the same protocol under /emby:
  WISP_NOTIFY_EMBY_URL: http://emby:8096
  WISP_NOTIFY_EMBY_API_KEY: <api-key>
```

### Plex

wisp discovers your library sections (cached for 5 minutes) and issues a partial
scan of just the folder that changed, in the section whose library path contains
it.

```yaml
environment:
  WISP_NOTIFY_PLEX_URL: http://plex:32400
  WISP_NOTIFY_PLEX_TOKEN: <x-plex-token>
```

Notification failures never prevent a pin or delete. Keep the AIOStreams plugin's
Wisp Pins polling source enabled as a recovery path. Treat webhook URLs and
tokens as passwords: do not publish them in screenshots or logs, and rotate them
if exposed.

## API

Add an episode:

```sh
curl -X POST http://localhost:8080/api/add -d '{
  "media_type": "series",
  "imdb_id": "tt38262097",
  "title": "The Villager of Level 999",
  "year": 2026,
  "season": 1,
  "episode": 4,
  "quality": "1080p"
}'
```

Add a movie: `"media_type": "movie"`, omit `season`/`episode`.

`quality` is optional. **Omit it** and wisp pins AIOStreams' top-ranked stream
and labels the file with whatever resolution it returned. **Set it** (`1080p`,
`2160p`/`4k`, …) and wisp pins a stream *of that resolution* — so you can pin
`1080p` and `2160p` of the same title as two distinct files. If AIOStreams has
streams but none at that resolution, the add returns `502 no_quality_match` (a
retriable "not yet" — the title stays worth re-adding).

Add failures carry a JSON `error` code so a feeder can tell "no stream yet" from
a misconfiguration: `502 no_streams` / `502 no_quality_match` (keep monitoring),
`500 aiostreams_auth` (bad credentials), `429 rate_limited` (throttled; honors
`Retry-After`), `503 upstream_unavailable` (transient).

#### Request-shaped add (async intake)

`POST /api/add` also accepts a **request-shaped** body that registers/updates a
monitor instead of pinning synchronously. wisp's scheduler then does enumeration,
release gating, and resolution in the background; the call returns `202` fast.
This is the contract a Silo plugin shim uses.

```sh
curl -X POST http://localhost:8080/api/add -d '{
  "media_type": "movie",
  "tmdb_id": "27205",
  "imdb_id": "tt1375666",
  "title": "Inception",
  "year": 2010,
  "is_anime": false,
  "qualities": [{"id": "1080p"}, {"id": "2160p", "is4k": true}],
  "request_ref": "silo-req-42"
}'
```

- `is_anime` (optional) fixes the category; omit it to let the heuristic decide
  (see [Library layout](#library-layout)).
- `qualities` (optional) is a list of `{id, is4k}` tiers; omit for best-available.
- `request_ref` (optional) is an opaque caller key echoed back by the status API.
- A body with `qualities`, `request_ref`, `is_anime`, or a tmdb-only identity is
  treated as request-shaped; the simpler `imdb_id` + `season`/`episode`/`quality`
  payload above still pins synchronously as before.

#### Request status

`GET /api/requests/status?media_type=&tmdb_id=` (falling back to `imdb_id`)
returns wisp's authoritative state for a title, for a request router to poll:

```json
{ "state": "queued", "pinned_qualities": ["1080p"], "detail": "awaiting home-media release" }
```

- `queued` — tracked, nothing in scope pinned yet (unreleased/unaired, or the
  released-but-no-stream-yet retry window). Unreleased is never `failed`.
- `completed` — requested scope pinned and servable (movie: a pin exists; series:
  every currently-aired episode is pinned — the monitor keeps running for future
  episodes but still reports `completed`).
- `failed` — permanent give-up only (unresolvable identity). wisp otherwise
  retries indefinitely. A `404` means wisp is not tracking the title.

List pins: `GET /api/pins`. Status: `GET /api/status`.

Remove a title:

```sh
# by virtual path
curl -X DELETE "http://localhost:8080/api/pins?path=shows/…/ep.mkv"
# by identity (all qualities)
curl -X DELETE http://localhost:8080/api/pins -d '{"imdb_id":"tt38262097","season":1,"episode":4}'
# by identity, one quality tier only
curl -X DELETE http://localhost:8080/api/pins -d '{"imdb_id":"tt38262097","season":1,"episode":4,"quality":"2160p"}'
```

### Requests & monitoring

In Seerr → **Settings → Notifications → Webhook**, set the **Webhook URL** to
`http://<wisp-host>:8080/api/seerr`, enable **Request Approved** + **Request
Automatically Approved**, and use this JSON payload:

```json
{
  "notification_type": "{{notification_type}}",
  "subject": "{{subject}}",
  "media": {
    "media_type": "{{media_type}}",
    "tmdbId": "{{media_tmdbid}}",
    "tvdbId": "{{media_tvdbid}}",
    "imdbId": "{{media_imdbid}}"
  },
  "request": { "request_id": "{{request_id}}" }
}
```

On approval wisp resolves the movie/series, pins what's aired, and monitors the
rest.

**`WISP_SEERR_URL` / `WISP_SEERR_API_KEY` are optional but recommended.** wisp
never polls Seerr — the webhook is the push. The catch is that the webhook can't
reliably carry two things: whether a request is **4K** (Overseerr exposes no
per-request 4K field in its webhook at all) and, depending on your Overseerr
version/template, the requested **seasons**. So when a webhook arrives, wisp makes
**one reactive call back to the Seerr API** to read that request's authoritative
`is4k` + seasons. That's the *only* thing the credentials do — there are no other
API calls, and nothing is polled.

Without them wisp still works, with two degradations:

- every request is treated as **1080p** (no HD/4K split), and
- a request fulfills the **whole show** rather than the specific seasons asked for.

Set them if you want the 4K tier split or per-season scoping; leave them blank if
you don't care about either.

You can also drive monitoring directly (Seerr optional):

```sh
# monitor a title (media-server-neutral)
curl -X POST http://localhost:8080/api/monitors \
  -d '{"media_type":"series","imdb_id":"tt38262097","title":"The Villager of Level 999","year":2026,"qualities":["1080p"]}'
curl http://localhost:8080/api/monitors                      # list the watchlist
curl -X DELETE "http://localhost:8080/api/monitors?key=series:tt38262097"
curl -X POST http://localhost:8080/api/monitors/refresh      # re-check now
```

Inspect the scheduler's plan with `GET /api/schedule`:

```sh
curl http://localhost:8080/api/schedule
```

It returns `interval_seconds` (the fallback re-check ceiling), `next_wake` (unix
time the scheduler's sleep timer is set to fire), and an `items` array. Each item
carries its `key`, `media_type`, `title`, `state`, `next_check` (its scheduled
re-check time; omitted once complete), `next_release` (the next known
release/airstamp, present only when `waiting`), `last_checked`, `last_error`,
requested `qualities`/`seasons`, how many files are `pinned`, and
`pending_targets` (requested quality tiers with nothing pinned yet).

`state` is one of:

- **waiting** — nothing due until a real future release date or airstamp.
- **retrying** — due again later only as a retry (no stream found yet, a
  metadata error, or no known upcoming episode).
- **pending** — due now; the next scheduler pass will try to pin it.
- **completed** — a movie whose every requested tier is pinned (kept as history).
- **paused** — monitoring disabled for this item.

## Configuration

| Env | Default | Notes |
|-----|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | **Required.** Your AIOStreams manifest URL (the uuid is read from it automatically) |
| `WISP_AIOSTREAMS_PASSWORD` | — | Your AIOStreams password. Required for the Search API's Basic auth **unless** your instance enables `allowUnauthenticatedSearchApiRequests` |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address |
| `WISP_DB_PATH` | `/data/wisp.db` | Pin + monitor database (persist this) |
| `WISP_MOUNT_PATH` | — | Self-mount here (needs `/dev/fuse` + `SYS_ADMIN`); unset = HTTP only |
| `WISP_SEERR_URL` | — | Optional but recommended. Overseerr/Jellyseerr base URL — only used to read a request's 4K flag + seasons back (reactively, per webhook; never polled). Blank = all requests treated as 1080p, whole-show |
| `WISP_SEERR_API_KEY` | — | Optional but recommended. Seerr API key for the enrichment above |
| `WISP_SCHEDULE_INTERVAL` | `2h` | Monitor re-check ceiling (it wakes near the next airstamp regardless) |
| `WISP_TMDB_API_KEY` | — | TMDB v3 key or v4 token — enables home-media release gating for movies |
| `WISP_TMDB_MARKETS` | `US,CA,GB,AU,DE,FR,IT,ES,JP,IN` | Regions whose digital/physical dates release a movie |
| `WISP_NOTIFY_ARR_WEBHOOK_URL` | — | ARR-compatible Autoscan webhook (Silo/Sonarr/Radarr) for instant import, rename, and delete updates |
| `WISP_SILO_WEBHOOK_URL` | — | Deprecated alias for `WISP_NOTIFY_ARR_WEBHOOK_URL` (canonical wins if both set) |
| `WISP_NOTIFY_JELLYFIN_URL` / `_API_KEY` | — | Jellyfin (or Silo Jellyfin-compat) base URL + admin API key; rescans via `Library/Media/Updated` |
| `WISP_NOTIFY_EMBY_URL` / `_API_KEY` | — | Emby base URL + API key (same protocol, routed under `/emby`) |
| `WISP_NOTIFY_PLEX_URL` / `WISP_NOTIFY_PLEX_TOKEN` | — | Plex base URL + `X-Plex-Token`; partial-scans the changed folder |
| `WISP_MOUNT_ALLOW_OTHER` | `true` | Let other UIDs read the mount |
| `WISP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
| `WISP_READ_CHUNK_SIZE` | `32M` | Initial VFS read chunk (smaller = less debrid over-fetch on seeks) |
| `WISP_READ_CHUNK_SIZE_LIMIT` | `512M` | Cap for the chunk ramp |

## Documentation

Full docs live in [`docs/`](docs/README.md): [Architecture](docs/Architecture.md) ·
[API](docs/API-Reference.md) · [Configuration](docs/Configuration.md) ·
[Deployment](docs/Deployment.md) · [Feeding wisp](docs/Feeding-wisp.md) ·
[Troubleshooting](docs/Troubleshooting.md).

## Status

Early but solid. The core — add/pin/serve, self-healing streams and mount,
CDN-cached fast starts, provider-id metadata tags, `/api/status` + `/metrics` —
works and is tested. See [docs/](docs/README.md).

## License

MIT
