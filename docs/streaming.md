# Streaming with mkvgo (HLS + DASH / CMAF)

mkvgo turns a media file into an adaptive-streaming presentation — **the
packaging, not the encoding**. It never transcodes: the compressed samples are
copied verbatim into fragmented-MP4 (CMAF) segments. Producing the encodes
(bitrate ladders, codec choices) remains a transcoder's job; mkvgo does
everything after that, in pure Go, with no ffmpeg.

This is the topic guide. For every flag see [cli.md](cli.md); for the Go types
see [library.md](library.md); for browser playback see [wasm.md](wasm.md).

---

## The model: one segment set, two manifests

That is the whole idea of CMAF, and what mkvgo delivers:

```
stream/
  master.m3u8        HLS manifest   (hls.js, Safari)   -.
  manifest.mpd       DASH manifest  (dash.js)           |  both describe the
  init.mp4                                               |  SAME CMAF segments
  seg00001.m4s  seg00002.m4s  ...                       -'
  audio1.m3u8  init_a1.mp4  seg_a1_00001.m4s  ...    each audio track (demuxed)
  sub1.m3u8  sub1.vtt  sub1_00001.vtt  ...           each subtitle track (WebVTT)
  iframe.m3u8                                        trick-play (video keyframes)
```

Tracks are **demuxed** — one rendition per track, as Apple recommends for HLS
and as DASH players require. A player picks its video, its audio language, and
its subtitles independently. Segments are cut on video keyframes so each is
independently decodable (a player starts at any segment).

```bash
mkvgo to-hls video.mkv -o stream/ -segment 6
```

---

## Two ways to serve it

Same output, two operating modes — and they emit **byte-identical** files, so
you can mix them freely (pre-generate popular titles, serve the rest on demand).

### Pre-generate everything — `to-hls`

Writes the whole presentation to a directory. Serve it with any static file
server. Simple, cacheable, CDN-friendly.

```bash
mkvgo to-hls video.mkv -o stream/
```
```go
err := mp4.RemuxToHLS(ctx, "video.mkv", "stream/", mp4.Options{SegmentMs: 6000})
```

### On demand — `hls-segment` / `mp4.PlanHLS`

Nothing is pre-generated: each resource is built **when a player requests it**.
First-play latency is milliseconds, storage cost is zero. A plan does a few
bounded reads up front (metadata + the seek index), then each segment reads
only its own window.

```bash
mkvgo hls-segment video.mkv master.m3u8        # → stdout
mkvgo hls-segment video.mkv seg00042.m4s -o -  # just that segment
```
```go
plan, _ := mp4.PlanHLS(ctx, "video.mkv", mp4.Options{SegmentMs: 6000})

// The declarative entry point: the name is exactly what a player requests.
data, contentType, _ := plan.Resource(ctx, "seg00042.m4s")
names := plan.Resources()        // every servable name
```

An HTTP handler is one call:

```go
http.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
    data, mime, err := plan.Resource(r.Context(), path.Base(r.URL.Path))
    if err != nil { http.NotFound(w, r); return }
    w.Header().Set("Content-Type", mime)
    w.Write(data)
})
```

> **Runnable example:** [`examples/hls-server`](../examples/hls-server/) is a
> complete on-demand server (this handler + a landing page that plays the
> output in hls.js and dash.js). `go run ./examples/hls-server -src movie.mkv`.

Combined with a remote source (see below), only the byte ranges a viewer
actually watches are ever transferred from storage.

---

## Sources

The packager accepts, transparently (format sniffed from the first bytes):

| Source | HLS/DASH | On-demand notes |
|---|:---:|---|
| **MKV / WebM** | ✅ | Plans through the **Cues** seek index — needs one (`mkvgo reindex` adds it). |
| **MP4 / MOV** | ✅ | The moov **sample table is the index** — the plan is *exact by construction*: every resource, master and MPD included, is byte-identical to the full pass. |
| **`http(s)://` URL** | ✅ (`hls-segment`) | Read over HTTP Range (`httpfs`) — package straight from S3, only the needed ranges transferred. |

```bash
mkvgo to-hls movie.mp4 -o stream/                 # MP4 source
mkvgo hls-segment https://nas/movie.mkv seg00003.m4s -o -   # remote, ranged
```

**Audio-only** sources (music, podcasts — no video track) package fine: the
first audio track becomes the primary rendition, boundaries follow its sample
grid, and the master carries no `RESOLUTION`.

---

## What's in a presentation

- **Video** — the primary rendition (`playlist.m3u8` / `init.mp4` / `seg*.m4s`),
  with the movie title, tags and **cover art** on its init segment.
- **Each audio track** — its own rendition (`audioN.m3u8` …), declared as an
  HLS `EXT-X-MEDIA` group and a DASH AdaptationSet, with language — so a VF/VO
  file gets native language selection.
- **Each text subtitle track** — a segmented **WebVTT** rendition
  (`subN.m3u8` + `subN.vtt`); SRT, WebVTT and ASS/SSA (flattened to plain text)
  are carried, bitmap subtitles are dropped with a reason.

---

## Adaptive bitrate — `to-abr`

Package several **pre-encoded** qualities of the same content into one
multi-variant master. mkvgo does not create the qualities (no transcode); it
packages the ones you give it.

```bash
mkvgo to-abr -o stream/ movie-1080p.mkv movie-720p.mkv movie-480p.mkv
```

The first source is the reference (its audio and subtitles serve every
variant); the others contribute their video. Each variant lands in `v1/`,
`v2/`, … and the top `master.m3u8` declares each with its real
`BANDWIDTH`/`RESOLUTION`/`CODECS`. For seamless switching the sources should
share the keyframe cadence (same GOP length).

---

## Gapless multi-file sessions (concat) - `concat-hls`

Play several files as **one continuous HLS session** instead of one
presentation per file: consecutive episodes, or any ordered set of sources,
served under a single `master.m3u8`/`playlist.m3u8` with no player reload and
no session boundary. This is not ABR (the sources are not quality variants of
the same content); it is a different presentation entirely, concatenated in
playback order.

```bash
mkvgo concat-hls -o stream/ ep01.mkv ep02.mkv ep03.mkv
```

Each source packages into its own `p0/`, `p1/`, `p2/` … exactly like `to-hls`
would on its own: no re-timestamping, no copy. The top-level playlists stitch
the parts together with `EXT-X-DISCONTINUITY` at each boundary, HLS's own
"new timeline starts here" signal: the mechanism that lets a player carry on
playing into the next part without reloading, and the reason each part's
segments stay byte-identical to its own standalone packaging.

**Compatibility.** Every source must share the same video codec family and
the same kept-audio layout (count, codec, language, in order); otherwise
`concat-hls`/`PlanConcat` refuse up front, before anything is written, with a
precise list of what differs. Subtitles are softer: they ride along only when
every source exposes the same rendition layout (count/language/name/forced);
otherwise they are dropped from the session (`Options.OnDrop`) while the
video/audio concatenation still plays. Where subtitles do ride along, their
cue times (unlike the CMAF fragments) are not reset by the discontinuity, so
each part's cues are shifted onto the concatenated timeline by the cumulative
duration of the parts before it.

```bash
mkvgo concat-segment master.m3u8 ep01.mkv ep02.mkv ep03.mkv   # → stdout, nothing pre-generated
```

`concat-segment` is the on-demand twin (`mp4.PlanConcat`), built the same way
`hls-segment`/`PlanHLS` are: a resource name in, its bytes out, nothing
pre-generated. Resource names are `master.m3u8`, `playlist.m3u8`,
`audioN.m3u8`, `subN.m3u8`/`subN.vtt` at the top, and `p{k}/<name>` (`p0/`,
`p1/`, …) for a specific part's own resource: the same names `concat-hls`'s
directories use.

v1 does not support `--aes-key`/`--single-file`, and emits no combined DASH
manifest (DASH shares one `SegmentTimeline` per AdaptationSet; independent
per-part timelines have nothing to share it over, exactly the `to-abr`
non-aligned rationale) and no combined I-frame playlist.

---

## Single-file byte-range — `--single-file`

Instead of hundreds of segment files, pack each rendition into **one**
progressive file (init + `sidx` index + all fragments) served by byte ranges:
HLS `EXT-X-BYTERANGE`, DASH on-demand `SegmentBase`. Friendlier to object
storage — the server only needs HTTP Range support.

```bash
mkvgo to-hls video.mkv -o stream/ --single-file
# → stream.mp4, stream_a1.mp4 (+ playlists) instead of seg*.m4s
```

The embedded fragments are byte-identical to the segmented mode's.

---

## Virtual versions — `--keep-tracks` / `--keep-lang`

Serve many **virtual versions of one file** — no copy, no re-mux, just a
different track subset per request. `KeepTracks` restricts the presentation to a
set of Matroska track IDs; the dropped renditions are simply never built.

```bash
# "VF only": keep the video and the French audio, drop everything else
mkvgo to-hls movie.mkv -o vf/ --keep-tracks 1,2

# by language (CLI sugar; keeps video + every track matching the codes):
mkvgo to-hls movie.mkv -o vo/  --keep-lang eng          # VO: video + English audio/subs
mkvgo to-hls movie.mkv -o clean/ --keep-tracks 1,2,6    # "clean": drop a forced/logo subtitle track

# on demand, same subset, nothing pre-generated:
mkvgo hls-segment movie.mkv master --keep-lang fre
```

A self-hosted server offers "VF / VO / clean / director's cut / camera angle N"
from a single stored file at **zero extra storage**, and switching versions is
just a different plan (near-zero latency). At least one video track must be kept
(HLS needs video). The on-demand plan applies the subset **byte-identically** to
the full pass, so pre-generated and on-demand serving still mix transparently.

`--keep-lang` is CLI convenience: a language can map to several tracks (e.g.
VFF + VFQ, both `fre`) and then all are kept — use `--keep-tracks` for exact
control. A library caller (Go, WASM) already has the track metadata from
`probe`, so it builds the ID list itself and passes `Options.KeepTracks`
(`{ keepTracks: [...] }` in WASM) — no ambiguity.

---

## Securing delivery

### AES-128 encryption

Encrypt every media segment (whole-segment AES-128-CBC, RFC 8216) and advertise
the key URI. Init segments and subtitles stay clear; the key is only pointed at,
never written to the output.

```bash
mkvgo to-hls video.mkv -o stream/ \
  --aes-key 00112233445566778899aabbccddeeff \
  --aes-key-uri https://api.example.com/key
```
```go
mp4.Options{Encrypt: &mp4.HLSEncryption{Key: key16, KeyURI: "https://…/key"}}
```

Read by hls.js. (This is self-hosted-content encryption, not studio DRM — see
*Non-goals* below.) AES-128 is HLS-only, so no DASH manifest is emitted for an
encrypted presentation.

### Signed / templated URLs

`RewriteURL` (CLI `--url-prefix`) rewrites every URI the playlists and the MPD
reference — a CDN base, or a per-URL signed token. Resource names stay
canonical: your server strips the decoration before calling `Resource`.

```go
mp4.Options{RewriteURL: func(name string) string {
    return signedCDNURL(name)   // e.g. name + "?token=" + hmac(name, expiry)
}}
```

---

## Trick-play (scrubbing)

Two complementary tools:

- **I-frame playlist** — `to-hls` emits `iframe.m3u8` (`EXT-X-I-FRAMES-ONLY`,
  declared in the master), one keyframe per segment as a byte range into the
  **existing** segments. Zero extra media; what hls.js/Safari use for smooth
  scrubbing previews. (MP4-source on-demand plans expose it too.)
- **`extract-frame`** — pull the keyframe nearest any timestamp as a
  decoder-ready file (Annex-B / IVF); decode it to an image with one ffmpeg
  call. A storyboard is a loop over the keyframe index.

```bash
mkvgo extract-frame movie.mkv 00:12:30 -o frame.h264
ffmpeg -i frame.h264 -frames:v 1 thumb.jpg
```

---

## Playing it in the browser

The WebAssembly build runs the same packager client-side. `openHLS(file)` opens
an on-demand plan over ranged reads of a local `File` — a huge file plays
through Media Source Extensions with **bounded memory**, no server, no upload.

```js
const plan = await MkvGo.openHLS(file, { segmentSeconds: 6 })
// feed plan.resource('init.mp4') then plan.segment(n) into an MSE SourceBuffer
```

Full browser guide, TypeScript wrapper and React hooks: **[wasm.md](wasm.md)**.

---

## Readiness check

Before serving, `validate` audits what streaming relies on — a present Cues
index, cues keyed on real video keyframes (not audio), subtitle durations,
frame-rate metadata:

```bash
mkvgo validate video.mkv        # exits non-zero on error-severity issues
```

---

## Non-goals (evaluated, deliberately out of scope)

- **Studio DRM (CENC / SAMPLE-AES / FairPlay/Widevine/PlayReady).** mkvgo does
  AES-128 for self-hosted content; multi-DRM signaling and per-sample
  encryption are a separate concern, out of scope here.
- **LL-HLS (low-latency).** A *live-ingest* mechanism (partial segments,
  blocking reloads) — mkvgo packages VOD files, where it changes nothing for
  the viewer.
- **Multi-period DASH.** Periods model *discontinuities* (ad insertion); using
  them for chapters would force decoder rebuilds at every mark and degrade
  seeking. Chapters ride in the progressive MP4 remux instead.

Rationale in [library.md](library.md#non-goals-of-the-streaming-stack-evaluated).
