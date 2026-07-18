# API Reference

Base URL: `http://<host>:8080` (default). All bodies are JSON.

wisp's API is intentionally tiny — it is *fed* by whatever you already use
(see [Feeding wisp](Feeding-wisp.md)). There is no auth on the API today; keep it
on a trusted network.

---

## `POST /api/add`

Resolve a title via AIOStreams, pin the top stream, and create its virtual file.

**Body**

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `media_type` | string | ✅ | `movie` or `series` |
| `imdb_id` | string | ✅ | Stremio id — `tt…` or `tmdb:…` |
| `title` | string | ✅ | Used for the folder/file name |
| `year` | number | – | Used for the folder/file name |
| `season` | number | series | |
| `episode` | number | series | |
| `quality` | string | – | Optional hint; wisp labels the file with the resolution **AIOStreams actually returned**, so this is usually unnecessary |

**Responses**

- `200` → `{"virtual_path": "...", "size": 1471496964}` — pinned.
- `502` → `no playable stream: ...` — AIOStreams has no stream yet. This is a
  normal "retry later" signal, not an error; a feeder should re-add on its next
  cycle.
- `400` — invalid body / missing required field.

```sh
curl -X POST http://localhost:8080/api/add -d '{
  "media_type":"series","imdb_id":"tt38262097",
  "title":"The Villager of Level 999","year":2026,"season":1,"episode":4
}'
```

---

## `GET /api/pins`

List every pin.

```json
[
  {
    "virtual_path": "shows/Show (2026)/Season 01/Show (2026) - S01E04 - [1080p].mkv",
    "media_type": "series", "imdb_id": "tt38262097",
    "season": 1, "episode": 4, "title": "Show", "year": 2026,
    "quality": "1080p", "size": 1471496964, "resolved_at": 1784345504
  }
]
```

Feeders use this to avoid re-adding episodes wisp already has.

---

## `DELETE /api/pins`

Remove a pin; its virtual file drops out of the mount.

- By path: `DELETE /api/pins?path=<virtual_path>`
- By identity: body `{"imdb_id":"tt…","season":1,"episode":4}` (omit season/episode for a movie; matches all pins for that id)

```sh
curl -X DELETE "http://localhost:8080/api/pins?path=shows/Show%20(2026)/Season%2001/ep.mkv"
curl -X DELETE http://localhost:8080/api/pins -d '{"imdb_id":"tt38262097","season":1,"episode":4}'
```

Response: `{"deleted": ["<virtual_path>", ...]}`.

With Wisp's self-mount enabled, deleting a media file directly from the mount
performs the same unpin operation as `DELETE /api/pins`.
The mount does not permit creating, modifying, or renaming media files.

---

## `GET /api/status`

```json
{ "version": "0.2.0", "uptime_seconds": 1234, "pins": 42,
  "mounted": true, "mount_path": "/mnt/wisp" }
```

## `GET /metrics`

Prometheus text-format metrics: `wisp_pins`, `wisp_mounted`, `wisp_uptime_seconds`,
`wisp_file_requests_total`, `wisp_link_cache_hits_total`,
`wisp_link_cache_misses_total`, `wisp_reresolves_total`, `wisp_link_cache_entries`.

## `GET /api/healthz`

`200 ok` — liveness probe.

---

## File serving (what the media server hits)

Everything not under `/api/` is the virtual filesystem:

- `GET /` and directory paths → HTML listings (what rclone's `:http:` backend walks).
- `HEAD /<virtual_path>` → the pinned size, no upstream call (cheap `stat`).
- `GET /<virtual_path>` (with `Range`) → range-proxied bytes from the pinned
  stream, with [self-heal](Architecture.md#the-self-heal-model) on a dead upstream.

You normally don't call these directly — the rclone mount does.
