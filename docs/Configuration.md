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
| `WISP_TMDB_API_KEY` | — | TMDB v3 key or v4 token. Enables home-media release gating for movies (digital/physical dates); without it, wisp falls back to Cinemeta's release date. |
| `WISP_TMDB_MARKETS` | `US,CA,GB,AU,DE,FR,IT,ES,JP,IN` | ISO-3166-1 regions whose TMDB digital/physical release makes a movie eligible (any one releasing counts). |
| `WISP_SILO_WEBHOOK_URL` | — | Optional ARR-compatible Silo Autoscan webhook. Sends import, rename, and file-delete events immediately. Keep this URL secret. |
| `WISP_MOUNT_ALLOW_OTHER` | `true` | Expose the mount to other UIDs — needed when a media-server container reads the mount as a different user. |
| `WISP_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. `debug` narrates every serve + the full self-heal path. |

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

## Silo webhook

Add an ARR-compatible webhook source in Silo Autoscan and set its generated URL
as `WISP_SILO_WEBHOOK_URL`. No event checkboxes or additional path settings are
required. wisp sends Import, Rename, and File Delete events and derives the
reported path from `WISP_MOUNT_PATH`, falling back to `/mnt/wisp`.

Delivery is best-effort: Silo or network failures are logged but do not fail the
Wisp operation. Leave the AIOStreams plugin's Wisp Pins polling source enabled
to recover any missed webhook. The generated URL contains a secret and should be
rotated if it is exposed.

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
