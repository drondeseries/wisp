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
- **Mount.** wisp self-mounts with embedded rclone; pins appear in a normal
  `movies/` and `shows/` layout that any media server scans.
- **Play.** On open, wisp range-proxies bytes from the pinned stream, which
  re-unlocks the debrid link on every request. If a link has died, wisp
  re-resolves through AIOStreams and playback self-heals.

Because the files carry real bytes, the media server reads real metadata
(codecs, duration, subtitles) and owns playback end to end — direct play,
transcode, and seeking all work.

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

## Instant Silo Autoscan

Create one ARR-compatible webhook source in **Silo → Autoscan → Sources**, copy
its webhook URL, and add it to wisp:

```yaml
environment:
  WISP_SILO_WEBHOOK_URL: https://silo.example.com/api/v1/autoscan/webhooks/<secret>
```

That is the only webhook setting. wisp automatically uses
`WISP_MOUNT_PATH` (default `/mnt/wisp`) when sending paths and notifies Silo
for:

- **Import** — immediately scans a newly pinned movie or episode.
- **Rename** — removes the previous path and scans the replacement when a
  re-resolve changes the virtual filename.
- **File Delete** — removes a deleted pin from Silo's library (via the API or a
  mounted `rm`).

Webhook failures never prevent a pin or delete. Keep the AIOStreams plugin's
Wisp Pins polling source enabled as a recovery path. Treat the webhook URL as a
password: do not publish it in screenshots or logs, and rotate it if exposed.

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
rest — 4K requests become a `[2160p]` file, standard requests `[1080p]`. Set
`WISP_SEERR_URL`/`WISP_SEERR_API_KEY` so wisp reads the request's seasons and 4K
flag authoritatively from the Seerr API (the webhook alone underspecifies them).

You can also drive monitoring directly (Seerr optional):

```sh
# monitor a title (media-server-neutral)
curl -X POST http://localhost:8080/api/monitors \
  -d '{"media_type":"series","imdb_id":"tt38262097","title":"The Villager of Level 999","year":2026,"qualities":["1080p"]}'
curl http://localhost:8080/api/monitors                      # list the watchlist
curl -X DELETE "http://localhost:8080/api/monitors?key=series:tt38262097"
curl -X POST http://localhost:8080/api/monitors/refresh      # re-check now
```

## Configuration

| Env | Default | Notes |
|-----|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | **Required.** Your AIOStreams manifest URL (the uuid is read from it automatically) |
| `WISP_AIOSTREAMS_PASSWORD` | — | Your AIOStreams password. Required for the Search API's Basic auth **unless** your instance enables `allowUnauthenticatedSearchApiRequests` |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address |
| `WISP_DB_PATH` | `/data/wisp.db` | Pin + monitor database (persist this) |
| `WISP_MOUNT_PATH` | — | Self-mount here (needs `/dev/fuse` + `SYS_ADMIN`); unset = HTTP only |
| `WISP_SEERR_URL` | — | Overseerr/Jellyseerr base URL (enables request enrichment) |
| `WISP_SEERR_API_KEY` | — | Seerr API key (authoritative seasons + 4K intent) |
| `WISP_SCHEDULE_INTERVAL` | `2h` | Monitor re-check ceiling (it wakes near the next airstamp regardless) |
| `WISP_TMDB_API_KEY` | — | TMDB v3 key or v4 token — enables home-media release gating for movies |
| `WISP_TMDB_MARKETS` | `US,CA,GB,AU,DE,FR,IT,ES,JP,IN` | Regions whose digital/physical dates release a movie |
| `WISP_SILO_WEBHOOK_URL` | — | Optional Silo Autoscan webhook for instant import, rename, and delete updates |
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
