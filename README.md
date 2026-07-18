# wisp

![CI](https://github.com/dreulavelle/wisp/actions/workflows/ci.yml/badge.svg)

A resolver-backed virtual filesystem for [AIOStreams](https://github.com/Viren070/AIOStreams).

wisp turns the streams AIOStreams selects into ordinary-looking media files. It
never downloads anything — each virtual file's bytes are range-proxied from the
resolved stream on demand. Point any media server (Silo, Plex, Jellyfin, Emby)
at the mount and it scans, probes, and plays them like local files.

> **Designed for AIOStreams.** wisp is built to run alongside
> [AIOStreams](https://github.com/Viren070/AIOStreams) and the
> [AIOStreams Silo plugin](https://github.com/drondeseries/silo-plugin-aiostreams),
> and it talks to AIOStreams' Search API directly (not the generic Stremio addon
> protocol) — so AIOStreams is required. That's not really a limit, though:
> AIOStreams is the aggregator, so whatever Stremio scrapers, debrid services, or
> usenet sources you configure *there* (Torrentio, Comet, MediaFusion, Easynews,
> …) all flow through to wisp. Any feeder that can POST to wisp's tiny
> [API](#api) works; the bundled Silo plugin is just the first one.

## How it works

- **Add** a movie or episode via the API. wisp asks AIOStreams for the best
  stream and pins the selection (URL + size).
- **Mount** wisp with rclone. The pinned files appear in a normal
  `movies/` and `shows/` layout.
- **Play.** On open, wisp range-proxies bytes from the pinned stream, which
  re-unlocks the debrid link on every request. If a link has died, wisp
  re-resolves through AIOStreams and playback self-heals.

Because the files carry real bytes, the media server reads real metadata
(codecs, duration, subtitles) and owns playback end to end — direct play,
transcode, and seeking all work.

## Quick start

wisp embeds rclone and self-mounts the library — one container, no separate
rclone process:

```yaml
services:
  wisp:
    image: ghcr.io/dreulavelle/wisp:latest
    container_name: wisp
    environment:
      WISP_AIOSTREAMS_URL: https://your-aiostreams/stremio/<uuid>/<config>/manifest.json
      WISP_AIOSTREAMS_PASSWORD: your-addon-password
      WISP_MOUNT_PATH: /mnt/wisp            # wisp mounts the library here
    volumes:
      - ./data:/data                        # persist the pin database
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

## Configuration

| Env | Default | Notes |
|-----|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | AIOStreams manifest URL (required) |
| `WISP_AIOSTREAMS_PASSWORD` | — | Addon password |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address |
| `WISP_DB_PATH` | `/data/wisp.db` | Pin database |
| `WISP_MOUNT_PATH` | — | Self-mount here (needs `/dev/fuse` + `SYS_ADMIN`); unset = HTTP only |
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
