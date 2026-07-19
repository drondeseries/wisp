# Configuration

All configuration is via environment variables.

| Variable | Default | Notes |
|----------|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | **Required.** Your AIOStreams manifest URL: `https://host/stremio/<uuid>/<config>/manifest.json` (or the alias form `.../stremio/u/<alias>/manifest.json`). |
| `WISP_AIOSTREAMS_PASSWORD` | — | Addon password. Paired with the UUID/alias from the URL for Search API auth. If it already contains `uuid:password`, it's used verbatim. |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address. |
| `WISP_DB_PATH` | `/data/wisp.db` | bbolt database for pins **and** monitors. Persist this (a volume) to keep your library and watchlist across restarts. |
| `WISP_MOUNT_PATH` | — | If set, wisp self-mounts the library here (needs `/dev/fuse` + `SYS_ADMIN`). Unset = serve HTTP only and mount it yourself. |
| `WISP_SCHEDULE_INTERVAL` | `2h` | Fallback ceiling for the monitor loop. The scheduler otherwise wakes near a monitored item's next known airstamp/release — it doesn't poll on a fixed tick. |
| `WISP_RESOLVE_CONCURRENCY` | `4` | How many episodes of a series resolve in parallel per scheduler pass. Titles are still processed one at a time, so this is the peak resolver fan-out against your debrid provider — raise it to drain long seasons faster, lower it if you hit rate limits. Clamped to `1`–`16`. |
| `WISP_TMDB_API_KEY` | — | TMDB v3 key or v4 token. Enables home-media release gating for movies (digital/physical dates); without it, wisp falls back to Cinemeta's release date. |
| `WISP_TMDB_MARKETS` | `US,CA,GB,AU,DE,FR,IT,ES,JP,IN` | ISO-3166-1 regions whose TMDB digital/physical release makes a movie eligible (any one releasing counts). |
| `WISP_NOTIFY_ARR_WEBHOOK_URL` | — | ARR-compatible Autoscan webhook (Silo/Sonarr/Radarr wire format) for instant import, rename, and delete rescans. Keep secret. |
| `WISP_SILO_WEBHOOK_URL` | — | Deprecated alias for `WISP_NOTIFY_ARR_WEBHOOK_URL` (canonical wins if both set). |
| `WISP_NOTIFY_JELLYFIN_URL` / `_API_KEY` | — | Jellyfin (or Silo's Jellyfin-compat) base URL + admin API key; rescans via `Library/Media/Updated`. |
| `WISP_NOTIFY_EMBY_URL` / `_API_KEY` | — | Emby base URL + API key (same protocol, routed under `/emby`). |
| `WISP_NOTIFY_PLEX_URL` / `WISP_NOTIFY_PLEX_TOKEN` | — | Plex base URL + `X-Plex-Token`; partial-scans just the changed folder. |
| `WISP_MOUNT_ALLOW_OTHER` | `true` | Expose the mount to other UIDs — needed when a media-server container reads the mount as a different user. |
| `WISP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` narrates every serve + the full self-heal path. |
| `WISP_READ_CHUNK_SIZE` | `32M` | Initial VFS read chunk (smaller = less debrid over-fetch on seeks). |
| `WISP_READ_CHUNK_SIZE_LIMIT` | `512M` | Cap for the read-chunk ramp. |

## AIOStreams URL & auth

You only fill in two things: `WISP_AIOSTREAMS_URL` (your manifest URL) and
`WISP_AIOSTREAMS_PASSWORD` (your AIOStreams password).

wisp uses the AIOStreams **Search API** (`/api/v1/search`), which authenticates
with HTTP Basic auth (`uuid:password`). wisp reads the **UUID from the
`/stremio/<uuid>/…` path** of the manifest URL automatically and pairs it with
`WISP_AIOSTREAMS_PASSWORD` — so the password is all you supply.

The password is required **unless** your AIOStreams instance has
`allowUnauthenticatedSearchApiRequests` enabled (check the instance's status
endpoint) — then you can leave `WISP_AIOSTREAMS_PASSWORD` unset. With auth
required and no password, wisp logs a startup warning and every add returns
`aiostreams_auth`.

> The alias form (`ALIASED_CONFIGURATIONS`) works for the manifest path, but the
> Search API expects the real UUID — use the full `/stremio/<uuid>/<config>/…`
> URL for `WISP_AIOSTREAMS_URL` to be safe.

Because wisp goes through your AIOStreams instance, **all of your AIOStreams
config applies automatically** — provider order, quality/HDR/dub preferences,
debrid vs usenet, filtering. wisp adds no ranking or filtering of its own.

## Persisting data

Mount a volume at `WISP_DB_PATH`'s directory (default `/data`). The pin database
is the whole library — lose it and you'd re-add everything (pins re-resolve
fine, but the list is gone).

## Media-server notifications

On every pin, rename, or delete, wisp tells your media server to rescan the
affected folder, so new content appears immediately. Configure any combination of
targets — all configured ones are notified. Paths are derived from
`WISP_MOUNT_PATH` (falling back to `/mnt/wisp`), the path the server sees on disk.

- **Silo (recommended)** — in **Autoscan → Sources**, add a *Sonarr/Radarr
  Webhook* source, click **Generate webhook URL**, and set it as
  `WISP_NOTIFY_ARR_WEBHOOK_URL`. No event checkboxes or path settings needed.
- **Jellyfin / Emby** — `WISP_NOTIFY_JELLYFIN_URL` / `WISP_NOTIFY_EMBY_URL` plus
  an admin API key; wisp posts a `Library/Media/Updated` hint.
- **Plex** — `WISP_NOTIFY_PLEX_URL` + `WISP_NOTIFY_PLEX_TOKEN`; wisp partial-scans
  just the changed folder.

Delivery is best-effort on a background goroutine: a slow or unreachable server is
logged but never blocks or fails a pin or delete. Webhook URLs and tokens are
secrets — rotate them if exposed.

## Mount tuning (self-mount mode)

wisp embeds rclone's VFS with sensible defaults for streaming: cache **off**
(pure passthrough, no local disk), a 32 MiB initial read chunk that ramps to
512 MiB for efficient sequential playback while keeping seeks snappy, and a
short directory cache. Set `WISP_READ_CHUNK_SIZE` (default `32M`) to tune the
initial chunk and `WISP_READ_CHUNK_SIZE_LIMIT` (default `512M`) to cap the ramp.
Smaller chunks reduce over-fetch after seeks; larger chunks favor sequential
throughput.

See [Deployment](Deployment.md) for the `/dev/fuse`, `SYS_ADMIN`, and mount
propagation requirements.
