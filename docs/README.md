# wisp

A resolver-backed virtual filesystem for [AIOStreams](https://github.com/Viren070/AIOStreams).

wisp turns the streams AIOStreams selects into ordinary-looking media files. It
never downloads anything — each virtual file's bytes are range-proxied from the
resolved stream on demand. Point any media server (Silo, Plex, Jellyfin, Emby)
at the mount and it scans, probes, and plays them like local files.

```
media server ──scans/reads──▶ wisp ──resolves via──▶ AIOStreams ──▶ debrid / usenet
   (plays)                   (VFS + self-heal)      (finds/ranks/parses)
```

The media server owns playback — planning, transcode, sessions, seeking. wisp is
the storage layer that makes a remote stream look like a local file. AIOStreams
does all the finding, ranking, filtering, and parsing. wisp glues the two.

## Pages

- **[Architecture](Architecture.md)** — how it works, the lifecycle, the self-heal model
- **[Configuration](Configuration.md)** — every environment variable
- **[API Reference](API-Reference.md)** — endpoints, payloads, responses
- **[Deployment](Deployment.md)** — compose, mount propagation, per-server library setup
- **[Feeding wisp](Feeding-wisp.md)** — how content gets in (Silo request router, plugin, direct)
- **[Troubleshooting](Troubleshooting.md)** — common issues

## In one breath

Add a title → wisp asks AIOStreams for the best stream and **pins** it (URL +
size) → a virtual file appears in the mount → on play, wisp range-proxies the
bytes and **self-heals** if the link dies. No downloads, real metadata, one
static binary.
