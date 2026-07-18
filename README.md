# wisp

A resolver-backed virtual filesystem for [AIOStreams](https://github.com/Viren070/AIOStreams).

wisp turns the streams AIOStreams selects into ordinary-looking media files. It
never downloads anything — each virtual file's bytes are range-proxied from the
resolved stream on demand. Point any media server (Silo, Plex, Jellyfin, Emby)
at the mount and it scans, probes, and plays them like local files.

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

```yaml
services:
  wisp:
    image: ghcr.io/dreulavelle/wisp:latest
    environment:
      WISP_AIOSTREAMS_URL: https://your-aiostreams/stremio/<uuid>/<config>/manifest.json
      WISP_AIOSTREAMS_PASSWORD: your-addon-password
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
```

```yaml
    # add to the service above to self-mount:
    devices:
      - /dev/fuse
    cap_add:
      - SYS_ADMIN
    environment:
      WISP_MOUNT_PATH: /mnt/wisp        # wisp mounts itself here
    volumes:
      - /mnt/wisp:/mnt/wisp:rshared     # propagate the mount to the host
```

wisp embeds rclone and mounts the library itself — no separate rclone
container or process. Point your media server's library at `/mnt/wisp`. Leave
`WISP_MOUNT_PATH` unset to serve HTTP only and mount it however you like.

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

## Configuration

| Env | Default | Notes |
|-----|---------|-------|
| `WISP_AIOSTREAMS_URL` | — | AIOStreams manifest URL (required) |
| `WISP_AIOSTREAMS_PASSWORD` | — | Addon password |
| `WISP_LISTEN_ADDR` | `:8080` | HTTP bind address |
| `WISP_DB_PATH` | `/data/wisp.db` | Pin database |
| `WISP_MOUNT_PATH` | — | Self-mount here (needs `/dev/fuse` + `SYS_ADMIN`); unset = HTTP only |
| `WISP_MOUNT_ALLOW_OTHER` | `true` | Let other UIDs read the mount |

## Status

Early. The core — add, pin, serve, self-heal, and in-process mount — works.
Watchlist sync and a status/metrics endpoint are next.

## License

MIT
