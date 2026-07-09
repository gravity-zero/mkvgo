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

**Serving over HTTP.** The hand-rolled handler above is the idea in five
lines; `mkvhttp.Handler(plan)` (package `github.com/gravity-zero/mkvgo/mkvhttp`)
is the batteries-included version -- strong ETag, conditional GET, Range,
per-resource `Cache-Control`, CORS -- since `mp4.HLSPlan`/`mp4.ABRPlan` already
satisfy its `Resolver` interface as-is. See `docs/library.md` for the full
semantics table. `mkvgo serve <file.mkv>` is this wired up as a CLI command
(see `docs/cli.md`).

Combined with a remote source (see below), only the byte ranges a viewer
actually watches are ever transferred from storage.

**Direct-play: serve the file as-is when the client supports it, no
packaging.** `mkvhttp.FileHandler(path)` serves one local file straight over
HTTP byte-range (streamed from an `*os.File`, no packaging, no decode) -- the
counterpart to `Handler`'s on-demand plans, for a client that can play the
source container/codecs natively. `mkvgo serve <file.mkv> --direct` wires
this up as a CLI command; `--auto` runs a `Playability` check first and picks
`--direct` or the HLS plan for you. See `docs/library.md` for the semantics
table.

---

## Play while downloading (growing files) -- `serve-growing` / `mp4.PlanGrowingHLS`

A file that is still being **written** -- a download landing on disk, streamed
in from another service -- can be served as HLS before it finishes: the media
playlist lengthens as new whole clusters arrive, then finalizes to a normal
VOD playlist once the source is complete. This is VOD-to-live, not live
ingest: there is no chunked transfer and no LL-HLS, only a regular,
progressively-written file.

The motivating case: a viewer opens an episode that a background job is still
downloading from a remote library. Instead of waiting for the whole file,
`PlanGrowingHLS` serves whatever has landed so far and keeps extending the
playlist as more of the file arrives, exactly like a live channel that happens
to have a known, finite end.

```bash
mkvgo serve-growing downloading.mkv -addr :8478 -segment 6
```
```go
plan, err := mp4.PlanGrowingHLS(ctx, "downloading.mkv", mp4.Options{SegmentMs: 6000})
for range time.Tick(time.Second) {
    added, err := plan.Refresh(ctx)   // scan whatever whole clusters landed
    if added > 0 {
        // NumSegments() grew; player poll picks the new EXTINF lines up
    }
}
// once the downloader signals "done" (or Refresh auto-detects it: a Cues
// index now parses, or a known-size Segment element's declared end is
// reached with its last cluster whole):
plan.Complete()
```

**Mechanics.**
- **Cursor, not an index.** A growing source rarely has a Cues index yet, so
  segment boundaries are discovered by scanning forward from the last
  confirmed cluster, cutting on video keyframes at `Options.SegmentMs` -
  the same rule `PlanHLS` applies to its Cues, just discovered live.
- **Partial-cluster rule.** A cluster whose header declares N bytes with only
  M<N present on disk is never scanned; the cursor stops before it and
  retries on the next `Refresh`. A segment is only published once its
  underlying clusters are each confirmed **whole**.
- **Byte-identity.** A published segment's bytes are guaranteed identical to
  what `PlanHLS`/`RemuxToHLS` would produce for the finished file - segment
  building is not reimplemented, the same code path runs against the same
  byte range. Segment numbering is stable: once published, a segment's byte
  range never changes.
- **EVENT, not a sliding window.** While growing, the media playlist carries
  `#EXT-X-PLAYLIST-TYPE:EVENT` and no `#EXT-X-ENDLIST` - append-only, the
  whole presentation retained from media sequence 0. This is deliberately
  **not** a live sliding window (which would evict old segments): the source
  will finish, so nothing here is actually unbounded.
- **Finalization.** `Complete()` (an explicit "the download is done" signal)
  or auto-detection during `Refresh` (a Cues index now parses, or a
  known-size Segment element's declared end has been reached with its last
  cluster whole) closes the final segment and switches the playlist to
  `#EXT-X-PLAYLIST-TYPE:VOD` + `#EXT-X-ENDLIST`.
- **v1 limits**, explicit rather than silent: Matroska/WebM sources only (no
  growing MP4 yet); a video track is required (an audio-only growing plan is
  refused, matching `PlanHLS`'s own Matroska path); no subtitle renditions, no
  DASH manifest, no I-frame trick-play playlist; `Options.Encrypt`/`CENC` are
  refused; only known-size clusters are supported (an unknown-size/streaming
  cluster is reported as an explicit error).

See `docs/library.md` for the full `GrowingHLSPlan` API and `docs/cli.md` for
`serve-growing`'s flags.

---

## Sources

The packager accepts, transparently (format sniffed from the first bytes):

| Source | HLS/DASH | On-demand notes |
|---|:---:|---|
| **MKV / WebM** | ✅ | Plans through the **Cues** seek index — needs one (`mkvgo reindex` adds it). |
| **MP4 / MOV** | ✅ | The moov **sample table is the index** — the plan is *exact by construction*: every resource, master and MPD included, is byte-identical to the full pass. |
| **`http(s)://` URL** | ✅ (`hls-segment`) | Read over HTTP Range (`httpfs`) -- package straight from a web/CDN origin, only the needed ranges transferred. |
| **`s3://bucket/key`** | ✅ (`hls-segment`) | Read via SigV4-signed Range requests (`s3fs`) -- same ranged behaviour, straight from S3 or an S3-compatible service. |

```bash
mkvgo to-hls movie.mp4 -o stream/                 # MP4 source
mkvgo hls-segment https://nas/movie.mkv seg00003.m4s -o -   # remote, ranged
mkvgo hls-segment s3://my-bucket/movie.mkv seg00003.m4s -o - # remote, ranged, S3
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

## Subtitle resync (virtual) -- `--sub-offset`

A viewer's subtitle drifts out of sync (a common re-encode/re-cut symptom);
`Options.SubtitleOffsetMs` (CLI `--sub-offset <ms>`) shifts every WebVTT cue by
that many milliseconds -- negative allowed -- with **no file rewritten**: a
re-plan with a new offset serves a different sync instantly.

```bash
mkvgo to-hls movie.mkv -o stream/ --sub-offset -350   # subtitles 350ms earlier
mkvgo hls-segment movie.mkv sub1_00003.vtt --sub-offset 500 -o -   # on demand
```
```go
mp4.Options{SubtitleOffsetMs: -350}
```

A cue whose shifted end lands at or before 0 is dropped; a cue straddling 0 is
clamped to start at 0. The windowed `subN_%05d.vtt` segment boundaries are
evaluated **after** the shift, so a cue lands in whichever segment its new
timing puts it in, and the on-demand plan stays byte-identical to the full
pass for the same offset. A zero offset (the default) reproduces today's
output exactly.

This only affects the WebVTT rendition pipeline (`subN.m3u8`/`subN.vtt`) that
`to-hls`/`PlanHLS` build; it has no effect on `to-mp4`'s native `tx3g`/`wvtt`
subtitle tracks -- a separate, muxed-into-the-container code path this option
does not touch.

---

## Chapter markers and ad-insertion points

`Options.ChapterMarkers` (CLI `--chapter-markers`) exposes the source's
Matroska/MP4 chapters as navigable markers in the HLS and DASH manifests --
**opt-in**, off by default. It never decodes, transcodes or re-segments the
media: only the playlist/manifest *text* gains lines, so the segments
(`.m4s`, `init.mp4`) are byte-identical whether the option is on or off.

```bash
mkvgo to-hls movie.mkv -o stream/ --chapter-markers
mkvgo hls-segment movie.mkv playlist --chapter-markers -o -
```
```go
mp4.Options{ChapterMarkers: true}
```

**HLS** -- the video media playlist (`playlist.m3u8`) gets one
`#EXT-X-DATERANGE` per chapter, in playlist order, right after
`#EXT-X-PLAYLIST-TYPE:VOD`:

```
#EXT-X-DATERANGE:ID="chapter-1",START-DATE="1970-01-01T00:00:00.000Z",DURATION=245.000,X-CHAPTER-TITLE="Intro"
```

`START-DATE` is computed from a fixed zero epoch (1970-01-01T00:00:00Z) plus
the chapter's start offset, so the same source always renders the same dates
-- there is no wall-clock dependency and no real "date" claim being made; it
is EXT-X-DATERANGE's required ISO-8601 timestamp used as a reproducible
timeline marker, exactly the mechanism players already use for chapter
navigation and mid-roll ad-insertion cue points. `DURATION` is the chapter's
length in seconds (the next chapter's start, or the presentation's end for
the last one, when the source leaves `ChapterTimeEnd` unset -- the common
case). Audio and subtitle renditions carry no DATERANGE lines.

**DASH** -- a `<EventStream>` on the `<Period>` (per ISO/IEC 23009-1 5.3.9,
EventStream is a Period child, not an AdaptationSet child), one `<Event>` per
chapter, millisecond timescale, the title as the event's text body:

```xml
<EventStream schemeIdUri="urn:mkvgo:dash:chapter:2024" timescale="1000">
  <Event id="1" presentationTime="0" duration="245000">Intro</Event>
</EventStream>
```

`schemeIdUri` is this package's own -- no chapter EventStream scheme is
IANA-registered; ISO/IEC 23009-1 leaves it application-defined. Both forms
are byte-identical between the full pass (`RemuxToHLS`, which writes
`manifest.mpd` alongside the HLS playlists) and the on-demand plan
(`PlanHLS`) for the same source and option, the same
byte-parity invariant every other packaging fact in this document holds to.
A source with no chapters emits nothing extra either way, option on or off.

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

### Common Encryption (CENC)

Sample-level encryption (ISO/IEC 23001-7), the packaging an EME-capable
player's own DRM path (Widevine/PlayReady/FairPlay CDM) consumes -- mkvgo does
the packaging only: no license server, no DRM handshake, the caller supplies
the key. Two schemes: `cenc` (AES-CTR, a per-sample IV) and `cbcs` (AES-CBC, a
constant IV, 1-encrypted:9-clear pattern on video). Unlike AES-128 above, a
CENC presentation still gets a DASH manifest (with a `ContentProtection`
element), and both HLS renditions (video and audio) carry `EXT-X-KEY`.

```bash
mkvgo to-hls video.mkv -o stream/ \
  --cenc-scheme cenc --cenc-key 00112233445566778899aabbccddeeff \
  --cenc-kid 00000000000000000000000000000001 --cenc-iv 0000000000000001 \
  --cenc-key-uri https://api.example.com/key
```
```go
mp4.Options{CENC: &mp4.CENCOptions{
    Scheme: "cenc", Key: key16, KeyID: kid16, IV: iv8, KeyURI: "https://…/key",
}}
```

Video must be H.264 or HEVC (subsample encryption: the 4-byte NAL length plus
the NAL header stay clear, the rest is protected). No I-frame playlist is
emitted (a ciphertext byte range is not independently decryptable). Full detail
-- exact clear/protected rules, IV derivation, key delivery -- in
[library.md](library.md#common-encryption-cenc).

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
  scrubbing previews. Both MKV/WebM and MP4/MOV sources get it, full pass and
  on-demand plan alike.
- **`extract-frame`** — pull the keyframe nearest any timestamp as a
  decoder-ready file (Annex-B / IVF); decode it to an image with one ffmpeg
  call. A storyboard is a loop over the keyframe index.

**On-demand cost.** An MP4 plan builds `iframe.m3u8` eagerly at `PlanHLS` time
-- the sample table already has every segment's exact sample count/size, so it
is free. A Matroska plan builds it **lazily**, on the first request for
`iframe.m3u8` (cached after that): `PlanHLS` itself still does only its usual
few bounded reads. The lazy pass walks the video track's block headers only --
sizes, timecodes, sync flags -- never the sample bytes, so its cost is bounded
by the video track's block *count*, not by any payload volume.

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

- **DRM license servers / multi-DRM key management (Widevine/PlayReady/
  FairPlay).** mkvgo packages sample-level Common Encryption (CENC, see above)
  with a caller-supplied key -- the box format an EME-capable player's own DRM
  CDM consumes. It does not talk to a license server or manage per-title keys;
  that integration (and any DRM system's own license-acquisition protocol)
  stays the deploying application's job.
- **LL-HLS (low-latency).** A *live-ingest* mechanism (partial segments,
  blocking reloads) — mkvgo packages VOD files, where it changes nothing for
  the viewer.
- **Multi-period DASH.** Periods model *discontinuities* (ad insertion); using
  them for chapters would force decoder rebuilds at every mark and degrade
  seeking. Chapters ride in the progressive MP4 remux instead.

Rationale in [library.md](library.md#non-goals-of-the-streaming-stack-evaluated).
