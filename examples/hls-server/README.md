# Example: on-demand HLS + DASH server

A complete, runnable streaming server in ~90 lines. It packages a media file
into CMAF **on the fly** with [`mp4.PlanHLS`](../../docs/streaming.md) - nothing
is pre-generated, and the same segments are served as both HLS and DASH.

## Run

```bash
# Local file (MKV/WebM or MP4/MOV):
go run ./examples/hls-server -src movie.mkv

# Remote source over HTTP Range (S3, NAS…) - only the watched bytes transfer:
go run ./examples/hls-server -src https://nas.local/movie.mp4 -addr :9000
```

Then open **http://localhost:8080/** and pick a player, or point any client at:

| URL | Player |
|---|---|
| `http://localhost:8080/hls/master.m3u8` | HLS - hls.js, Safari |
| `http://localhost:8080/hls/manifest.mpd` | DASH - dash.js |

Flags: `-src` (required), `-addr` (default `:8080`), `-segment` seconds
(default `6`).

## What to look at

The entire integration is the handler:

```go
plan, _ := mp4.PlanHLS(ctx, src, opts)          // once per title

http.HandleFunc("/hls/", func(w, r) {
    name := path.Base(r.URL.Path)               // master.m3u8, seg00042.m4s, sub1.vtt…
    h, err := plan.Open(r.Context(), name)
    // …Content-Type h.ContentType(), Content-Length h.Size(), then h.WriteTo(ctx, w)
})
```

`plan.Open(name)` serves whatever a player asks for - playlists, the DASH
manifest, init segments, media segments, audio renditions, subtitle WebVTT, the
I-frame playlist - reading only that resource's bytes. Its exact size is known
before the first byte, and a media segment is written from the source as it
goes, so a client costs a pooled buffer, never a segment. The bytes are those
of `plan.Resource(name)`, which returns the resource whole. The plan is
immutable and safe for concurrent requests; a production server caches one plan
per title (keyed by source URL).

## Going further

- **Encryption / signed URLs** - add `Encrypt` and `RewriteURL` to `opts`
  (see [streaming.md](../../docs/streaming.md#securing-delivery)).
- **ABR** - build several `PlanHLS` for pre-encoded qualities, or pre-generate
  a variant master with `mp4.RemuxToABR`.
- **Browser-only** (no server) - the WebAssembly build packages client-side;
  see [`web/example/`](../../web/example/) and [wasm.md](../../docs/wasm.md).
