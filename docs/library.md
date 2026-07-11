# Library Usage Guide

The complete API guide. New here? The **[recipes](recipes.md)** are a gentler,
task-first starting point (CLI and Go side by side).

## Package Overview

| Package | Import | Purpose |
|---|---|---|
| `mkv` | `github.com/gravity-zero/mkvgo/mkv` | Core types, FS port, EBML IDs |
| `mkv/reader` | `github.com/gravity-zero/mkvgo/mkv/reader` | Parse MKV files into `Container` |
| `mkv/writer` | `github.com/gravity-zero/mkvgo/mkv/writer` | Write `Container` to MKV bytes |
| `mkv/ops` | `github.com/gravity-zero/mkvgo/mkv/ops` | High-level operations |
| `mkv/subtitle` | `github.com/gravity-zero/mkvgo/mkv/subtitle` | SRT/ASS parsing |
| `matroska` | `github.com/gravity-zero/mkvgo/matroska` | Facade -- re-exports everything |
| `ebml` | `github.com/gravity-zero/mkvgo/ebml` | Low-level EBML codec |

`matroska` and `mp4` are the stable public API -- import them for most use cases; both are held additive and backward-compatible across 0.x releases. The `mkv`, `mkv/reader`, `mkv/writer`, `mkv/ops` and `mkv/subtitle` packages are lower-level and experimental: their APIs may change between minor versions. Import them directly when you need capabilities the facade does not expose (streaming, `NewWebMStreamWriter`).

Operations process the container incrementally -- they read and write block by block (or cluster by cluster) and never hold the whole file in memory, so multi-gigabyte inputs run with bounded memory.

---

## Reading MKV Metadata

**From a file path:**
```go
import "github.com/gravity-zero/mkvgo/matroska"

c, err := matroska.Open(ctx, "video.mkv")
if err != nil { return err }

fmt.Println(c.Info.Title)
fmt.Println(c.DurationMs, "ms")

for _, t := range c.Tracks {
    fmt.Printf("#%d %s %s lang=%s\n", t.ID, t.Type, t.Codec, t.Language)
}
```

**From an io.ReadSeeker (in-memory, HTTP, etc.):**
```go
import "github.com/gravity-zero/mkvgo/mkv/reader"

c, err := reader.Read(ctx, myReadSeeker, "label.mkv")
```

**Block-level access (frame iteration):**
```go
f, _ := os.Open("video.mkv")
defer f.Close()

br, err := matroska.NewBlockReader(f, c.Info.TimecodeScale)
if err != nil { return err }

for {
    block, err := br.Next()
    if err == io.EOF { break }
    if err != nil { return err }
    // block.TrackNumber, block.Timecode, block.Keyframe, block.Data
}
```

Laced blocks (real muxers pack fixed-rate audio as N frames under ONE stored
timecode) are delivered one frame at a time; with the track's DefaultDuration
known, frame i is stamped `blockTS + i×duration` (and `block.BlockTimecode`
keeps the lace's shared value). A sequential reader picks the durations up on
its own while walking over the Tracks element; a reader started mid-file
(`reader.NewBlockReaderAt`) never sees it, so pass them explicitly:

```go
br.SetTrackDefaultDurations(matroska.TrackDefaultDurations(c.Tracks))
```

Without a known duration every frame of a lace keeps the block timecode
(`Track.DefaultDurationNs` is 0 when the source declares none).

---

## Writing MKV Files

Write a `Container` to any `io.Writer`:

```go
import "github.com/gravity-zero/mkvgo/mkv/writer"

var buf bytes.Buffer
err := writer.Write(&buf, container)
```

---

## WebM Output

WebM is a constrained Matroska profile: the `webm` DocType and a small codec set (VP8/VP9/AV1 video, Vorbis/Opus audio, WebVTT subtitles).

Check whether a `Container` can be written as WebM:

```go
if err := matroska.ValidateWebM(container); err != nil {
    // Names each track whose codec is outside the WebM subset, or which is
    // missing mandatory init data (Opus OpusHead, Vorbis headers, AV1 av1C).
    return err
}
```

`matroska.WriteWebM` writes the `webm` DocType (version 4 when an AV1 track is present, else 2) plus Info and Tracks. Like `writer.Write`, it writes metadata only -- no clusters.

For a complete, playable WebM with frames, remux a source file:

```go
err := matroska.RemuxToWebM(ctx, "in.mkv", "out.webm")
```

`RemuxToWebM` validates the codecs, copies every block verbatim into time-bounded `webm` clusters, and rejects sources with non-WebM codecs. The output is a seekable file: it carries a Cues index and a SeekHead. Elements outside the WebM subset (Chapters, Attachments, Tags) are dropped; list them beforehand with `matroska.WebMNonSubsetElements(container)`.

To write a WebM stream live (no source file), use `writer.NewWebMStreamWriter` -- the WebM counterpart of `NewStreamWriter`.

---

## MP4 Remux

The `mp4` package remuxes between Matroska/WebM and MP4 (ISO base media file format) without transcoding. It is isolated from the EBML core -- it shares no low-level code with `ebml`/`mkv`.

```go
import "github.com/gravity-zero/mkvgo/mp4"
```

### MKV/WebM → MP4

```go
err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4")
```

Each track's compressed samples are copied verbatim into MP4 sample tables. Supported codecs:

- **Video:** H.264, HEVC, AV1, VP9 (`vp09`; the `vpcC` comes from the CodecPrivate or the first keyframe's header).
- **Audio:** AAC, Opus, AC-3, E-AC-3, FLAC, MP3, DTS (incl. DTS-HD; carried as `mp4a`/`esds`).
- **Subtitles:** SRT (`S_TEXT/UTF8`) is carried as `tx3g` timed text. Bitmap/styled formats (PGS, VOBSUB, ASS) are dropped.

By default an unsupported audio/video codec (e.g. TrueHD) aborts the remux, so the output never silently omits content. Set `Options.SkipUnsupported` to drop such tracks instead; each dropped track is reported via `Options.OnDrop`:

```go
err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4", mp4.Options{
    SkipUnsupported: true,
    OnDrop: func(d mp4.DroppedTrack) {
        log.Printf("dropped track %d (%s): %s", d.ID, d.Codec, d.Reason)
    },
})
```

Other behaviour:

- **B-frame** reordering is preserved via a signed `ctts` box.
- **Colour/HDR** code points are written as a `colr` (nclx) box.
- **Chapters** are written both as a Nero `chpl` box and as a QuickTime chapter track linked from the media tracks with `tref`/`chap`.
- `moov` is placed after `mdat` by default. Set `Options.FastStart` to write `moov` first (one extra pass over the media, via a temporary file), for progressive HTTP playback.

Memory use scales with the sample count, not the file size: sample data is streamed to `mdat` while only the sample tables are held in memory.

### MKV/WebM → fragmented-MP4 HLS

> For the full picture — the CMAF model, on-demand serving, ABR, security,
> trick-play, remote sources — see the **[streaming guide](streaming.md)**.
> This section is the API reference.

```go
err := mp4.RemuxToHLS(ctx, "in.mkv", "stream/", mp4.Options{SegmentMs: 6000})
```

Writes a fragmented-MP4 / CMAF presentation into the output directory, served through two manifests over the same segments: HLS (`master.m3u8` + per-rendition media playlists) and DASH (`manifest.mpd`). Tracks are demuxed — the video rendition (`playlist.m3u8`, `init.mp4`, `seg00001.m4s` …) plus one rendition per audio track (`audio1.m3u8`, `init_a1.mp4`, `seg_a1_00001.m4s` …, an `EXT-X-MEDIA` AUDIO group / one MPD AdaptationSet each) — so multi-audio sources get native language selection. Each `.m4s` is `styp` + `moof` + `mdat`; each init is ftyp + moov with `mvex`/`trex` and empty sample tables (movie metadata — title, tags, cover art — rides on the video init). No transcoding — samples are copied verbatim, so the codec set matches `RemuxToMP4`.

Segments are cut on video keyframes at roughly `Options.SegmentMs` (default 6 s) and each is independently decodable, so a player seeks by fetching the segment plus `init.mp4`. Text subtitle tracks (SRT, WebVTT, ASS/SSA flattened) become segmented WebVTT renditions declared in the master playlist; bitmap subtitles are dropped (reported via `Options.OnDrop`). This is the CMAF *copy rung* of an HLS ladder — the packaging; bitrate variants (real ABR) remain a transcoder's job.

Memory is bounded regardless of file size: per-sample metadata is held in RAM (the same order the progressive muxer holds) while sample bytes are streamed through one temp file per track and read back sequentially as the segments are written.

### On-demand HLS (`mp4.PlanHLS`)

```go
plan, err := mp4.PlanHLS(ctx, "in.mkv", mp4.Options{SegmentMs: 6000})

// The declarative entry point: name → bytes + Content-Type. The name is
// exactly the URI a player requests, so an HTTP handler is one call:
data, mime, err := plan.Resource(ctx, "seg00042.m4s")
plan.Resources()                    // every servable name (playlists, init, segments, subtitles)

// Granular accessors when you want them:
plan.MasterPlaylist(); plan.MediaPlaylist(); plan.InitSegment()
data, err = plan.Segment(ctx, n)    // the n-th (0-based) media segment
vtt, err := plan.Subtitle(ctx, i)   // the i-th subtitle rendition as WebVTT
plan.NumSegments(); plan.SegmentName(n)
```

```go
// A complete HLS server endpoint:
http.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
    data, mime, err := plan.Resource(r.Context(), path.Base(r.URL.Path))
    if err != nil { http.NotFound(w, r); return }
    w.Header().Set("Content-Type", mime)
    w.Write(data)
})
```

The zero-storage counterpart of `RemuxToHLS`. Sources may be Matroska/WebM or
MP4/MOV (sniffed). A Matroska plan performs a few bounded reads (the metadata
head with its Cues, the first and last clusters) and each `Segment(n)` seeks
its window through the Cues; an MP4 plan is **exact by construction** — the
moov sample table is the index, so every resource (master, DASH manifest,
I-frame playlist included) is byte-identical to the full pass, and `Segment`
reads just its samples' byte ranges. A server answers any HLS request in
milliseconds with nothing pre-generated; combined with an `httpfs` source,
only the ranges a viewer actually watches are ever transferred.

The fragments are built by the same code as `RemuxToHLS`, so every resource is
**byte-identical** to the full pass (regression-tested) — pre-generated and
on-demand serving mix transparently. Cover art and global tags ride in the
init segment (`WithAttachments`/`WithTags` reach them through the SeekHead,
still head-only). Text subtitle tracks are declared in the master playlist and
served as segmented WebVTT renditions (`subN.m3u8` + windowed `subN_00001.vtt`…,
plus the whole track as `subN.vtt`). Text blocks carry no cue index, so the
cues come from scanning the clusters — incrementally and bounded, with the
results cached in the plan: sequential playback advances a resumable prefix
scan (one bounded read per segment), and a far seek jumps through the segment
index to a bounded window, so any windowed request costs O(window) like a
video segment; only the whole-track `subN.vtt` costs a full pass. The seek
fast path assumes self-contained cues (explicit block durations, as real
muxers write); tracks with duration-less or over-long cues are served through
the always-exact prefix scan. The remaining difference: the master
`BANDWIDTH` is estimated from cluster sizes. The source must carry a Cues
index. The plan is immutable and safe for concurrent `Segment` calls.

### Play while downloading (`mp4.PlanGrowingHLS`)

```go
plan, err := mp4.PlanGrowingHLS(ctx, "downloading.mkv", mp4.Options{SegmentMs: 6000})

added, err := plan.Refresh(ctx)   // scan any whole new clusters that landed
plan.NumSegments()                // grows as segments close
plan.MediaPlaylist()              // EVENT, no ENDLIST, until finalized

plan.Complete()                   // or let Refresh auto-detect completion
plan.MediaPlaylist()              // now VOD + ENDLIST

// Resource/Segment/MasterPlaylist/InitSegment/NumSegments/Resources mirror
// HLSPlan, serving only segments fully covered by data seen so far.
data, mime, err := plan.Resource(ctx, "seg00003.m4s")
```

HLS over a Matroska/WebM source that may still be being **written** (a
download landing on disk): the media playlist is `EVENT`-typed and lengthens
as new whole clusters arrive, then finalizes to `VOD`+`ENDLIST` once the
source completes. This is VOD-to-live, not live ingest - no chunked transfer,
no LL-HLS, just a resumable cursor over a regular file.

**The EVENT->VOD contract.**
`#EXT-X-PLAYLIST-TYPE:EVENT` and no `#EXT-X-ENDLIST` while growing (append-
only, media sequence 0 forever - this is **not** a live sliding window: the
whole presentation is retained since the source is known to be finite).
`Complete()` - an explicit "the download finished" signal, since the source
itself may never write a trailer - or auto-detection during `Refresh` (a
Cues index now parses, or a known-size Segment element's declared end is
reached with its last cluster whole) switches to
`#EXT-X-PLAYLIST-TYPE:VOD` + `#EXT-X-ENDLIST` and fixes every duration
exactly (a tail peek over the last, previously-open segment, the same
derivation `PlanHLS` performs over a complete file).

**The partial-cluster rule.** A cluster whose header declares N bytes with
only M<N currently on disk is a partial trailing cluster: it is never
scanned, and the cursor stops right before it, retrying on the next
`Refresh`. A segment is only published once every cluster it draws from is
confirmed **whole** - this is the correctness-critical rule that makes the
byte-identity guarantee possible.

**Byte-identity guarantee.** A published segment's bytes are guaranteed
identical to what `PlanHLS`/`RemuxToHLS` would produce for the finished file:
`GrowingHLSPlan` does not reimplement segment building - it keeps an internal
`HLSPlan` whose bounds/offsets/tracks are extended in place as the cursor
scans further, and `Segment`/`Resource` delegate straight to the same
`HLSPlan.Segment`/internal segment builder `PlanHLS` uses. Segment numbering
is stable: a published segment's byte range never changes across later
`Refresh` calls or finalization. The init segment's duration fields
(mvhd/tkhd/mdhd/mehd) read 0 (the standard "unknown duration" live-HLS
convention - players time playback off each fragment, never off the init)
while growing, and the exact totals once finalized, at which point the init
is byte-identical to a `PlanHLS` build of the same, now-finished file.

**v1 limits**, explicit rather than silent:
- Matroska/WebM sources only (no growing MP4 yet).
- A video track is required - an audio-only growing plan is refused, matching
  `PlanHLS`'s own Matroska path.
- No subtitle renditions, no DASH manifest, no I-frame trick-play playlist.
- `Options.Encrypt`/`Options.CENC` are refused.
- Only known-size clusters are supported; an unknown-size (streamed) cluster
  is reported as an explicit error rather than mishandled.
- The source must already carry its EBML+Segment head and a Tracks element
  when `PlanGrowingHLS` is called (the downloader writes those first).

**Concurrency.** `Refresh` and `Resource`/`Segment` may run on different
goroutines (a server polling `Refresh` while requests hit `Resource`); every
exported method takes the plan's own lock for its whole body, so this is
safe by construction, at some cost to concurrent-request throughput.

`mkvgo serve-growing <file.mkv>` is this wired up as a CLI command (see
`docs/cli.md`); see `docs/streaming.md` for the "play while downloading" use
case in more depth.

### Trick-play (I-frame playlists)

`RemuxToHLS` emits `iframe.m3u8` (`EXT-X-I-FRAMES-ONLY`, declared in the master
as `EXT-X-I-FRAME-STREAM-INF`) whenever the presentation has video and is not
encrypted: one keyframe per segment, referenced by `EXT-X-BYTERANGE` into the
**existing** segment files -- no extra media. Both MKV/WebM and MP4/MOV sources
get it, full pass and on-demand plan alike.

```go
plan, _ := mp4.PlanHLS(ctx, "movie.mkv", mp4.Options{SegmentMs: 6000})
data, mime, err := plan.Resource(ctx, "iframe.m3u8")
```

**Plan-time cost.** An MP4 plan builds `iframe.m3u8` eagerly, at `PlanHLS`
time: the moov sample table already has every segment's exact sample count,
sizes and sync flags, so it costs nothing extra. A Matroska plan instead
builds it **lazily** -- the first `Resource(ctx, "iframe.m3u8")` call performs
a one-time, cached pass over the video track and every later call is served
from that cache -- so `PlanHLS` itself keeps its usual few bounded reads
regardless of whether trick-play is ever requested. The lazy pass walks the
video track's block headers only (size, timecode, sync flag) via a
structure-only `BlockReader` mode (`SetHeaderOnly`) that seek-skips an
unlaced block's payload instead of reading it -- real muxers never lace
video, so this is the effective cost for real content: bounded by the
video track's block *count*, never by any payload volume. The result is
byte-identical to `RemuxToHLS`'s `iframe.m3u8` for the same source and
`SegmentMs`.

### Virtual Edit Layer (`Options.KeepTracks`)

```go
// One source file → many virtual versions, no copy, no re-mux:
vf,  _ := mp4.PlanHLS(ctx, "movie.mkv", mp4.Options{SegmentMs: 6000, KeepTracks: []uint64{1, 2}})    // "VF only"
vo,  _ := mp4.PlanHLS(ctx, "movie.mkv", mp4.Options{SegmentMs: 6000, KeepTracks: []uint64{1, 4, 9}}) // VO + English subs
```

`KeepTracks` restricts a presentation to a subset of the source's Matroska track
IDs. `PlanHLS`/`RemuxToHLS`/`RemuxToABR` then package only those tracks — the
dropped renditions are simply never built, so a self-hosted server serves "VF
only", "VO + English subs", "clean" (drop a logo/forced track) or a chosen
camera angle from one file, at zero storage and near-zero latency to switch
versions (just a different plan). At least one video track must be kept (HLS
needs video); the on-demand plan applies the same subset byte-identically to the
full pass. It composes with `VideoOnly` and rides through `RemuxToABR` per
variant.

### Subtitle resync (`Options.SubtitleOffsetMs`)

```go
mp4.Options{SubtitleOffsetMs: -350} // subtitles 350ms earlier, no file rewritten
```

Shifts every text-subtitle cue's timing by this many milliseconds (negative
allowed) in every WebVTT output `RemuxToHLS`/`PlanHLS` emit -- full pass
(`subN_*.vtt` files and playlists) and on-demand plan resources alike, MKV and
MP4 sources alike. A cue whose shifted end lands at or before 0 is dropped; a
cue straddling 0 is clamped to start at 0. Segment/window boundaries for the
windowed `subN_%05d.vtt` playlists are evaluated **after** the shift, so a cue
lands in whichever segment its new timing puts it in and the on-demand plan
stays byte-identical to the full pass for the same offset. `0` (the default)
reproduces today's output exactly -- a pure regression guard.

This is a per-session, virtual resync: no file is touched, and a server can
re-plan with a different offset instantly (e.g. per-viewer subtitle delay
preference). It only affects the WebVTT rendition pipeline `RemuxToHLS`/
`PlanHLS` build; `RemuxToMP4`/`RemuxFromMP4`'s native `tx3g`/`wvtt` subtitle
tracks are a separate, muxed-into-the-container code path this option does
not touch -- and HLS itself never takes that path, since it always carries
subtitles as their own WebVTT rendition.

### Chapter markers (`Options.ChapterMarkers`)

```go
mp4.Options{ChapterMarkers: true}
```

Opt-in (off by default). When the source carries chapters, `RemuxToHLS`/
`PlanHLS` expose them as navigable markers: one `#EXT-X-DATERANGE` per
chapter in the video media playlist (`START-DATE` from a fixed zero epoch +
the chapter's start offset, `DURATION` in seconds, `X-CHAPTER-TITLE` the
escaped title), and one `<Event>` per chapter in a `<EventStream>` on the DASH
manifest's `<Period>` (millisecond `presentationTime`/`duration`, the title as
the event body). Neither form touches the media: segments are byte-identical
whether the option is on or off, and both forms are byte-identical between
`RemuxToHLS` and `PlanHLS` for the same source and option. A source with no
chapters emits nothing extra either way.

### Thumbnails / storyboards (`matroska.ExtractKeyframeSample`)

```go
ks, err := matroska.ExtractKeyframeSample(ctx, "movie.mkv", 12*60*1000+30*1000)
// ks.PtsMs (the actual keyframe time), ks.Codec, ks.Ext (".h264"/".hevc"/".ivf"),
// ks.Data — Annex-B with parameter sets prepended (H.264/HEVC) or IVF (VP8/VP9/AV1)
```

Seeked through the Cues (bounded reads — works over `httpfs` too), never
decoded: pipe `ks.Data` to a decoder (`ffmpeg -i - -frames:v 1 thumb.jpg`) for
the image. A storyboard is this in a loop over `Container.Keyframes`.

### Single-file byte-range serving

`Options.SingleFile` packs each rendition into one progressive file (init +
`sidx` + all CMAF fragments): the HLS playlists reference it with
`EXT-X-BYTERANGE` and the DASH manifest switches to the on-demand profile
(`SegmentBase`/`indexRange`). The embedded fragments are byte-identical to
the segmented mode's `.m4s` files. `RemuxToHLS`/`RemuxToABR` only;
incompatible with `Encrypt` and `CENC`.

### ABR light (`mp4.RemuxToABR`)

```go
err := mp4.RemuxToABR(ctx, []string{"1080p.mkv", "720p.mkv"}, "stream/", mp4.Options{SegmentMs: 6000})
```

Packages pre-encoded quality variants (best first) into one multi-variant HLS
master — the packaging half of ABR, no transcoding. The first source is the
reference (audio + subtitles for every variant, from `v1/`); the others are
packaged `Options.VideoOnly`. Every variant gets its real
`BANDWIDTH`/`RESOLUTION`/`CODECS` in the top `master.m3u8`. Sources should share
GOP cadence for seamless switching. A combined **DASH** `manifest.mpd` (one
video AdaptationSet, a Representation per variant) is written at the top level
too **when the variants are segment-aligned** (DASH shares one SegmentTimeline
across a switch set); otherwise only each variant's own `v{k}/manifest.mpd` is
written. Variants may be Matroska/WebM or progressive/**fragmented** (CMAF)
MP4 — a pre-encoded ladder already in fragmented MP4 is read directly.

### On-demand ABR (`mp4.PlanABR`)

```go
plan, err := mp4.PlanABR(ctx, []string{"1080p.mp4", "720p.mp4"}, mp4.Options{SegmentMs: 6000})
data, contentType, err := plan.Resource(ctx, "v2/seg00042.m4s")
```

The on-demand counterpart of `RemuxToABR` (as `PlanHLS` is to `RemuxToHLS`):
nothing is pre-generated. `plan.Resource(ctx, name)` builds `"master.m3u8"`,
`"manifest.mpd"` (the combined DASH manifest, present only for segment-aligned
variants) or any `"v{k}/<name>"` when asked, byte-identical to the file
`RemuxToABR` would have written; `plan.Resources()` lists them all. One handler serves the whole
ladder — ideal for a per-request media server, and with an httpfs source each
variant transfers only the ranges a viewer watches. In the browser, the WASM
`openABR(inputs[])` exposes the same over `Uint8Array` or `Blob` variants (see
wasm.md).

### Forensic A/B session watermarking (`mp4.PlanWatermark`)

```go
wm, err := mp4.PlanWatermark(ctx, "titleA.mkv", "titleB.mkv", mp4.Options{SegmentMs: 6000})
seg, err := wm.Segment(ctx, n, sessionBit(n)) // A or B for this viewer
```

Two GOP-aligned encodes of one title (A and B, encoded so their segment
boundaries, timeline and decoder config match but their samples differ
imperceptibly) are served as ONE ordinary HLS presentation whose per-segment
bytes are drawn from A or B according to a per-viewer bit pattern. A leaked
copy - even re-recorded off a screen and re-compressed - then carries a binary
signature identifying the session that played it. No re-encode: `PlanWatermark`
verifies the two variants are spliceable (identical init, same segment count and
durations) and routes each segment to A or B; the manifest is identical for
every viewer, so one plan serves everyone and only the per-segment A/B bit
differs. `wm.SegmentForPattern(ctx, n, pattern)` reads bit n of a session's code
directly (LSB-first per byte).

mkvgo supplies the mechanism; the **code assignment** - which session gets which
pattern, and collusion-resistant codes (e.g. Tardos) so colluding leakers cannot
erase or frame - is policy the caller owns and passes in at serve time. The WASM
`openWatermark(a, b)` and the CLI `watermark-segment` expose the same. Encryption
is not combined with watermarking in this version (encrypt the served bytes at
the edge).

### Gapless multi-file sessions (`mp4.RemuxConcatToHLS` / `mp4.PlanConcat`)

```go
err := mp4.RemuxConcatToHLS(ctx, []string{"ep01.mkv", "ep02.mkv", "ep03.mkv"}, "stream/", mp4.Options{SegmentMs: 6000})

plan, err := mp4.PlanConcat(ctx, []string{"ep01.mkv", "ep02.mkv", "ep03.mkv"}, mp4.Options{SegmentMs: 6000})
data, contentType, err := plan.Resource(ctx, "p1/seg00042.m4s")
```

Plays several sources - e.g. consecutive episodes - as one continuous HLS
session: a single `master.m3u8`/`playlist.m3u8`/`audioN.m3u8`[/`subN.m3u8`]
spanning every source, with no player reload and no session boundary. This is
the concatenation twin of `RemuxToABR`/`PlanABR`: a composition of per-source
`HLSPlan`s, not quality variants of the same content.

Each source packages into its own `p0/`, `p1/`, `p2/` … exactly like
`RemuxToHLS`/`PlanHLS` would on its own: no re-timestamping, no copy. The
concatenated playlists reference those parts' segments directly, with
`EXT-X-DISCONTINUITY` marking each boundary - HLS's own "new timeline starts
here" signal, and the reason a part's segments stay byte-identical to its own
standalone packaging whether played concatenated or alone.

**Compatibility contract.** Every source must share the same video codec
family (same RFC 6381 codec string prefix) and the same kept-audio layout
(count, codec, language, in order); `RemuxConcatToHLS`/`PlanConcat` check this
from track metadata alone, before anything is written or planned, and refuse
with a precise error listing every mismatch otherwise. Subtitles are softer:
they ride along only when every source exposes the same rendition layout
(count/language/name/forced) - checked via `Options.OnDrop` otherwise, leaving
the video/audio concatenation intact. Where subtitles do ride along, their
cue times are shifted onto the concatenated timeline by the cumulative
duration of the sources before them - the one place concat rewrites content,
since WebVTT cue times (unlike the CMAF fragments) are not reset by
`EXT-X-DISCONTINUITY`.

`ConcatPlan.Resource(ctx, name)` builds `"master.m3u8"`, `"playlist.m3u8"`,
`"audio{j}.m3u8"`, `"sub{j}.m3u8"`/`"sub{j}.vtt"`, or any `"p{k}/<name>"`
(`k` 0-based) for a specific part's own resource, byte-identical to the file
`RemuxConcatToHLS` would have written; `ConcatPlan.Resources()` lists them
all. `ConcatPlan.NumParts()`/`ConcatPlan.Part(k)` expose the underlying
per-source `HLSPlan`s directly.

**v1 limits.** `Options.Encrypt` and `Options.SingleFile` are refused
explicitly. No combined DASH manifest is emitted: DASH shares one
`SegmentTimeline` per AdaptationSet, and independent per-part timelines have
nothing to share it over (the same non-aligned rationale as `RemuxToABR`'s
combined manifest). No combined I-frame playlist either; each part's own
trick-play, if any, is not carried forward into the concatenated session.

### Non-goals of the streaming stack (evaluated)

Two features were evaluated and deliberately not implemented:

- **LL-HLS (low-latency HLS).** Partial segments, blocking playlist reloads
  and preload hints exist to shave seconds off a *live* glass-to-glass
  latency. mkvgo packages *files* (VOD): every playlist is `PLAYLIST-TYPE:
  VOD` with `ENDLIST`, where LL-HLS mechanics add complexity and change
  nothing for the viewer. A live ingest pipeline (encoder → packager) is a
  different product; the CMAF fragment builders here would be reusable if
  one ever existed.
- **Multi-period DASH.** Periods model *discontinuities* (ad insertion,
  splicing). Mapping chapters onto periods forces players to tear down and
  rebuild decoders at every chapter mark — worse seeking for zero viewer
  benefit. Chapters are already carried in the progressive MP4 remux
  (`chpl` + QuickTime chapter track); players read chapter UIs from there,
  and DASH VOD stays single-period.

### Securing HLS delivery

```go
opts := mp4.Options{
    Encrypt: &mp4.HLSEncryption{
        Key:    key,                                  // 16 bytes; never written to the output
        KeyURI: "https://api.example.com/key?title=42", // what EXT-X-KEY advertises
    },
    RewriteURL: func(name string) string {            // URL templating / token signing
        mac := hmac.New(sha256.New, secret)
        mac.Write([]byte(name + expiry))
        return name + "?e=" + expiry + "&sig=" + hex.EncodeToString(mac.Sum(nil))
    },
}
```

`Encrypt` AES-128-encrypts every media segment (whole-segment CBC, PKCS#7,
IV = segment sequence — RFC 8216) in both `RemuxToHLS` and `PlanHLS`; the two
modes produce identical ciphertext. Init segments and subtitles stay clear;
the DASH manifest is withheld (AES-128 is an HLS mechanism). The key is only
ever advertised (`KeyURI`), never stored — authenticating that endpoint is the
server's access control.

**Key rotation** (forward secrecy). Set `RotateEverySegments` and a `Keys`
`[]HLSKey` list instead of the single `Key`: the key changes every N segments,
cycling through `Keys`, so a captured key decrypts only its own period rather
than the whole video. The media playlist then carries a fresh `EXT-X-KEY` at
each boundary and each segment is encrypted with its period's key. The schedule
is a pure function of the segment index, so `RemuxToHLS` and `PlanHLS` still
agree byte for byte.

```go
Encrypt: &mp4.HLSEncryption{
    RotateEverySegments: 10, // new key every 10 segments, cycling through Keys
    Keys: []mp4.HLSKey{
        {Key: keyA, KeyURI: "https://api.example.com/key/a"},
        {Key: keyB, KeyURI: "https://api.example.com/key/b"},
    },
},
```

`RewriteURL` rewrites every URI the playlists and the MPD reference. Resource
names stay canonical: the server strips its decoration (query token, prefix)
before calling `plan.Resource(ctx, name)`, and verifies the signature — the
hook makes every segment URL individually signed and expirable.

### Common Encryption (CENC)

```go
opts := mp4.Options{
    CENC: &mp4.CENCOptions{
        Scheme: "cenc",                        // or "cbcs"
        Key:    key,                            // 16 bytes; never written to the output
        KeyID:  kid,                            // 16 bytes; tenc's default_KID
        IV:     iv,                             // cenc: 8 or 16 bytes; cbcs: 16 (a full AES block)
        KeyURI: "https://api.example.com/key",  // what an EME-capable player resolves via its DRM path
        PSSH:   [][]byte{myPSSHBox},            // optional, copied verbatim into the init's moov
    },
}
```

Sample-level Common Encryption (ISO/IEC 23001-7) for the fMP4/HLS/DASH
pipeline: packaging only, with a caller-supplied key -- no license server, no
DRM handshake. Two schemes:

- **`"cenc"`** -- AES-CTR, a per-sample Initialization Vector (the caller's base
  IV plus the sample's absolute decode timestamp -- computed identically by
  `RemuxToHLS` and `PlanHLS`, so the ciphertext is byte-identical between the
  two). No pattern: every protected byte is encrypted.
- **`"cbcs"`** -- AES-CBC with a constant IV and, on video, a
  1-encrypted:9-clear 16-byte-block pattern per NAL unit's protected region
  (CBC state resets at each NAL); audio has no pattern (whole sample
  encrypted). A trailing partial block (< 16 bytes) is always left clear.

Subsample encryption (video) keeps the bytes a decoder/CDM must read in the
clear and protects the rest, per each codec's convention:

- **H.264/HEVC** (length-prefixed NALs): per NAL unit the clear region is the
  4-byte length field plus the NAL header (1 byte H.264, 2 bytes HEVC); the
  rest is protected.
- **AV1**: the OBU header, its leb128 size and the `frame_header_obu()` bits
  (rounded up to a byte) stay clear, the tile data is protected. Combined
  `OBU_FRAME` is handled by parsing `frame_header_obu()` against the segment's
  sequence header (a per-segment stateful parse - the segment opens on a
  keyframe carrying the sequence header). `OBU_TILE_GROUP` payloads are
  protected whole (single-tile assumption).
- **VP9**: each frame's uncompressed header stays clear, the compressed header
  and tile data are protected; a superframe index is clear. Inter frames that
  reuse a reference frame's dimensions are resolved from the segment keyframe
  (per-segment stateful parse).

All four decrypt and decode in a real EME player (verified with Shaka Player +
Clear Key in headless Chromium). AV1/VP9 fail loud rather than mis-protect on
the frame constructs the parsers do not yet cover (e.g. multi-tile tile groups,
some AV1 short-signalled reference frames), so a stream that cannot be split
correctly is refused, never served clear-but-signalled-protected. Audio (and
any non-subsample codec) is encrypted whole-sample. The init segment's
sample entry is wrapped `avc1`/`hvc1`/`mp4a`/… → `encv`/`enca` with a `sinf`
box (`frma`/`schm`/`schi`>`tenc`); each fragment carries `senc`/`saiz`/`saio`
describing the per-sample auxiliary information.

Signaling: HLS media playlists (video and audio) carry `EXT-X-KEY`
(`METHOD=SAMPLE-AES-CTR` for cenc, `METHOD=SAMPLE-AES` for cbcs,
`KEYFORMAT="identity"`); no I-frame playlist is emitted (a ciphertext byte
range is not independently decryptable). Unlike `Encrypt`, a CENC presentation
still gets a `manifest.mpd`, with a `ContentProtection` element
(`urn:mpeg:dash:mp4protection:2011`, carrying `cenc:default_KID`) on the video
and audio AdaptationSets.

Mutually exclusive with `Encrypt` and (in this version) `SingleFile`.
`KeyURI` left empty falls back to a `data:` URI embedding `Key` directly --
convenient for local testing, but it puts the raw key in the playlist text;
production deployments should always set a real `KeyURI`.

### MP4 → MKV

```go
err := mp4.RemuxFromMP4(ctx, "in.mp4", "out.mkv")
```

Reads `avc1`/`avc3`, `hvc1`/`hev1`, `av01`, `vp09`, `mp4a` (AAC, MP3 or DTS, by `esds` object type), `Opus`, `ac-3`, `ec-3`, `fLaC`, `tx3g` (→ SRT) and `wvtt` (→ WebVTT). Colour code points, chapters, the movie title (`udta/meta/ilst/©nam` → `Info.Title`), the other global tags (`©ART`/`©alb`/`©gen`/… → `Tags`) and per-track names (`udta/name`/`hdlr` → `Track.Name`) round-trip back to Matroska — and back out to MP4 with `RemuxToMP4`. Audio decodes bit-identically across the round trip for AAC/AC-3/E-AC-3/FLAC; Opus and MP3 stay in sync (delay handled by the decoder from the bitstream). Tracks with any other sample entry, and non-audio/video/subtitle tracks, are dropped.

#### Subtitles

`RemuxToMP4` never lets a subtitle block the remux:

- **SRT** (`S_TEXT/UTF8`) and **WebVTT** (`S_TEXT/WEBVTT`, and the WebM-era `D_WEBVTT/*` ids) are carried as `tx3g` timed text by default — the only MP4 subtitle form read universally (ffmpeg included). Inline markup is stripped.
- **`Options.NativeWebVTT`** carries WebVTT losslessly as native `wvtt` (ISO/IEC 14496-30) instead: cue settings and markup are preserved and Apple/Safari/CMAF read it, but ffmpeg's MP4 demuxer does not — so it is opt-in.
- **`Options.FlattenStyledSubs`** carries ASS/SSA (no native MP4 form) as `tx3g`, stripping the dialogue framing and override tags. Lossy: styling/positioning/karaoke is discarded. Without it, ASS/SSA are dropped (reported via `OnDrop`).
- Bitmap subtitles (PGS/VOBSUB) have no MP4 timed-text form and are dropped.

`Options` also carries `FS` (custom filesystem) and `Progress` (callback), like the other operations. A fully-specified call:

```go
err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4", mp4.Options{
    FastStart:       true, // moov before mdat (progressive HTTP)
    SkipUnsupported: true, // drop e.g. TrueHD instead of failing
    Progress: func(done, total int64) {
        fmt.Printf("\r%d / %d bytes", done, total)
    },
    OnDrop: func(d mp4.DroppedTrack) {
        fmt.Printf("dropped track %d (%s): %s\n", d.ID, d.Codec, d.Reason)
    },
})
```

The same two operations are exposed on the CLI as `mkvgo to-mp4` and
`mkvgo from-mp4` (see [cli.md](cli.md)).

### Probe MP4 metadata (no remux)

To read an MP4's codecs, colour, chapters and duration without converting it, use the metadata-only probe — the counterpart of `matroska.OpenMeta` for MKV. It parses only the `moov` box and never reads sample data (`mdat`) or writes an output file, so it is fast and bounded regardless of file size — the path to use when indexing or scanning a library.

```go
c, dropped, err := mp4.OpenMeta(ctx, "video.mp4")  // *mkv.Container, []mp4.DroppedTrack
fmt.Println(c.DurationMs, "ms")
for _, t := range c.Tracks {
    fmt.Printf("  #%d %s %s %s default=%v\n", t.ID, t.Type, t.Codec, t.Language, t.IsDefault)
}
for _, d := range dropped { // cover art / non-media tracks not in c.Tracks
    fmt.Printf("  dropped #%d %s: %s\n", d.ID, d.Codec, d.Reason)
}
```

The second return value lists tracks present in the file but not in `c.Tracks` — cover art / attached pictures and non-media tracks (hint, timecode, metadata) — so a probe can account for every stream ffprobe reports; it is nil when every track was carried.

`OpenMetaWithFS(ctx, path, fs)` runs it against a custom filesystem, and `ReadMeta(ctx, r, path)` reads from an `io.ReadSeeker` (the `moov` box may sit after the media, so seeking is required). Info, Tracks, Chapters, DurationMs and (for MP4) file-level Tags / `Info.Title` are populated; Attachments and Cues are left nil. The probe and `RemuxFromMP4` build their metadata from the same code, so they report identical tracks, chapters and duration.

#### Track metadata fields

Each `Track` carries the stream metadata ffprobe reports, read head-only from the container headers and codec configuration (no frame decode). Most map directly to an ffprobe `-show_streams` field; the derived display strings are helper methods, mirroring `ColorSpaceName()` etc.

| `Track` field / method | ffprobe field | Source |
|---|---|---|
| `Codec` / `CodecLongName()` | `codec_name` / `codec_long_name` | container codec id |
| `Language`, `LanguageBCP47` | `tags:language` | `mdhd`/`elng`, Matroska language |
| `IsDefault`, `IsForced` | `disposition` | `tkhd` flags / DASH-role `kind`; Matroska flags |
| `DurationMs` | per-stream `duration` | MP4 `mdhd` (per-track; 0 for Matroska) |
| `Bitrate` | `bit_rate` | MP4 `btrt` / esds `avgBitrate` |
| `Width`, `Height` | `width`, `height` | sample entry / `PixelWidth` |
| `DisplayAspectRatio()`, `SampleAspectRatio()` | `display_aspect_ratio`, `sample_aspect_ratio` | MP4 `pasp` / Matroska `DisplayWidth` / H.264-HEVC VUI `aspect_ratio_info` (bounded `av_reduce` like ffmpeg) |
| `Rotation` | Display Matrix side data | MP4 `tkhd` matrix (0/90/180/270) |
| `FrameRate` | `r_frame_rate` | MP4 `stts` (timescale ÷ first delta) / Matroska `DefaultDuration` |
| `AvgFrameRate()` | `avg_frame_rate` | frame count ÷ duration (MP4 video; 0 for Matroska) |
| `FrameCount` | `nb_frames` | MP4 `stsz` count (0 for Matroska) |
| `Profile`, `Level` | `profile`, `level` | SPS / hvcC |
| `PixelFormat` | `pix_fmt` | chroma subsampling + bit depth (SPS / av1C / vpcC) |
| `FieldOrder` | `field_order` | Matroska `FlagInterlaced` / H.264 `frame_mbs_only_flag` |
| `VideoBitDepth` | bit depth | `colr`/Colour or codec bitstream |
| `ColorSpaceName()`, `ColorTransferName()`, `ColorPrimariesName()`, `ColorRangeName()`, `IsHDR()` | `color_space`/`color_transfer`/`color_primaries`/`color_range` | `colr` (nclx/nclc) / Matroska Colour / SPS VUI |
| `DolbyVision` | side data | MP4 `dvcC`/`dvvC` box / Matroska `BlockAdditionMapping` |
| `HDR` (`MaxCLL`/`MaxFALL`, `MasteringDisplay`) | side data (Content light level / Mastering display metadata) | Matroska Colour `MaxCLL`/`MaxFALL`/`MasteringMetadata` / MP4 `clli`/`mdcv` |
| `StereoMode` (`StereoModeName()`), `Projection` | Stereo 3D / Spherical Mapping side data | Matroska `StereoMode`/`Projection` / MP4 `st3d`/`sv3d` |
| `Bitrate` | `bit_rate` | MP4 `btrt`/`esds` / Matroska `BPS` tag |
| `HearingImpaired`, `VisualImpaired`, `TextDescriptions`, `Original`, `Commentary` | `disposition.*` | Matroska Flag* elements (Matroska only) |
| `Channels` / `ChannelLayout()` | `channels` / `channel_layout` | codec config (HE-AACv2 PS counted) |
| `SampleRate`, `OutputSampleRate`, `EffectiveSampleRate()` | `sample_rate` | sample entry / Matroska; SBR output rate from the AAC ASC or `OutputSamplingFrequency` |

A few cases are genuinely not readable head-only (the data lives only in the media frames, so ffprobe decodes a frame): implicit in-band SBR / Parametric Stereo, and colour carried only in an in-band SPS. In those the probe reports the header value (e.g. the AAC core rate) rather than guessing. See the [CHANGELOG notes](../CHANGELOG.md).

**Colour determinacy.** `Track.ColourDetermined` reports whether the colour was actually read from a source — the container Colour element, an MP4 `colr` box, or the codec bitstream's colour signalling (H.264/HEVC VUI, AV1 `color_config`, VP9 `vpcC`) — *even when it resolves to "unspecified"* (every `Color*` left nil). It lets a caller tell a confirmed-SDR/unspecified stream (`true`, no colour values) from one whose colour could not be read at all (`false`): only the latter warrants a fallback. A 10-bit SDR stream whose SPS says `colour_description_present_flag = 0` is `ColourDetermined == true` with nil `Color*` — confirmed SDR, not "unread".

**HDR10 static metadata.** `Track.HDR` (`*HDRStaticMetadata`) carries the Content Light Level (`MaxCLL`/`MaxFALL`, cd/m²) and the SMPTE ST 2086 Mastering Display colour volume (`MasteringDisplay`: R/G/B + white-point CIE 1931 chromaticities and the luminance range) — the side data ffprobe reports for HDR10. Read head-only from the Matroska Colour element (`MaxCLL`/`MaxFALL` + `MasteringMetadata`) or the MP4 `clli`/`mdcv` boxes (whose fixed-point, G,B,R-ordered values are normalised to the Matroska units), nil when absent. Independent of `IsHDR()` (transfer-based detection) and `DolbyVision`.

**In-band colour fallback (opt-in).** Some streaming-style HEVC muxes keep the SPS in-band (a bare hvcC with no parameter sets) and write no container colour, so a head-only probe sees no colour at all. Passing the in-band option makes the probe — only for such a track — read its first sample, parse the SPS VUI, and apply an Alternative Transfer Characteristics SEI override (HLG's `bt2020-10` → `arib-std-b67` compatibility signal). It is off by default; tracks that already carry colour in the header read no frame.

```go
// MKV
c, _ := matroska.OpenMeta(ctx, "video.mkv", matroska.WithInBandColourFallback())
// MP4
c, _, _ := mp4.OpenMeta(ctx, "video.mp4", mp4.Options{InBandColour: true})
```

**Per-track bitrate (opt-in).** ffmpeg/mkvmerge write a per-track `BPS` tag — the per-track bitrate ffprobe surfaces as `TAG:BPS` (ffprobe's own `bit_rate` field stays `N/A` for Matroska, so this gives *more* than ffprobe; for MP4 `Track.Bitrate` comes from `btrt`/`esds` and *does* equal ffprobe's `bit_rate`). The metadata probe normally stops before the Matroska `Tags` element, so `Track.Bitrate` is nil for MKV. Passing the bitrate option follows the head `SeekHead` straight to the `Tags` element — one seek, no Cluster scan, since the muxer references `Tags` from the head — and fills `Track.Bitrate` from `BPS`, matching a full `Read`:

```go
c, _ := matroska.OpenMeta(ctx, "video.mkv", matroska.WithBitrate())
```

### Keyframe index (head-only)

To align `-c copy` HLS/DASH segments on source keyframes without a full packet scan, read `Container.Keyframes` — the metadata probe fills it in the same pass, no separate call or second open:

```go
// MKV/WebM: filled from the Cues index in the normal metadata pass.
c, err := matroska.OpenMeta(ctx, "video.mkv")
ks := c.Keyframes                                  // []int64 ms, ascending, de-duplicated

// MKV/WebM with no Cues: build the complete index in-process (no ffprobe).
c, _ = matroska.OpenMeta(ctx, "no-cues.mkv", matroska.WithKeyframeIndex())
ks = c.Keyframes

// MP4: opt-in, since building the sample table dominates the parse on a long movie.
c, _, err := mp4.OpenMeta(ctx, "video.mp4", mp4.Options{Keyframes: true})
ks = c.Keyframes
```

`Keyframes` holds the video track's keyframe presentation timestamps in milliseconds. MKV/WebM fills it from the `Cues` element reached via the `SeekHead` (one seek, no `Cluster` scan) in the normal metadata pass, and a full `matroska.Read` exposes it too. MP4 derives it from the `stss`/`stts`/`ctts` sample tables with the edit list (`elst`) applied as ffmpeg does — but only when `Options{Keyframes: true}` is set, because expanding the sample table is the dominant cost of parsing a long movie's `moov`; the default `mp4.OpenMeta` reads only the box headers and leaves `Keyframes` nil. It is nil when the source has no usable index.

**Cues-less Matroska.** Some muxers ship MKV/WebM with no `Cues`, so `Keyframes` is nil after the head-only pass. Rather than fall back to an external probe, two opt-in options recover it: `WithKeyframeIndex()` builds the **complete** index (every keyframe, equal to `ffprobe -skip_frame nokey`) in one sequential read-ahead pass over the Segment — header-only, no demux/decode, video keyframes only (SimpleBlock keyframe flag, or a BlockGroup with no `ReferenceBlock`); `WithSampledKeyframes(n)` is the cheaper coarse variant (one keyframe per sampled interval). Files that already carry `Cues` are never scanned. The CLI `keyframes` command uses `WithKeyframeIndex()` automatically.

### Subtitles to WebVTT

To serve a text subtitle as WebVTT without forking `ffmpeg -f webvtt`, extract straight to an `io.Writer` (e.g. an HTTP response):

```go
// Embedded track (by Container.Tracks ID), MKV/WebM or MP4:
err := ops.ExtractSubtitleWebVTT(ctx, "movie.mkv", trackID, w)
err = mp4.ExtractSubtitleWebVTT(ctx, "movie.mp4", trackID, w)

// External sidecar (.srt / .ass / .ssa / .vtt):
err = subtitle.FileToWebVTT("subs.fr.srt", w)
```

S_TEXT/UTF8 (srt) and S_TEXT/WEBVTT pass through; S_TEXT/ASS and `.ass` files are flattened to plain text (override tags dropped, `\N`/`\h` converted). Cue ends come from the BlockDuration / sample duration, falling back to the next cue's start. The lower-level pieces — `subtitle.Cue`, `WriteWebVTT`, `SRTToCues`, `ASSToCues`, `ResolveCueEnds` — are exported for custom pipelines.

---

## Mux / Demux

**Mux** -- combine tracks from multiple sources:

```go
err := matroska.Mux(ctx, matroska.MuxOptions{
    OutputPath: "output.mkv",
    Tracks: []matroska.TrackInput{
        {SourcePath: "video.mkv", TrackID: 1},
        {SourcePath: "audio.mkv", TrackID: 1, Language: "eng", Name: "Stereo", IsDefault: true},
    },
    Title:       "My movie",   // optional
    Chapters:    chapters,     // optional
    Tags:        tags,         // optional
    Attachments: attachments,  // optional
})
```

Mux writes the metadata you pass (title/chapters/tags/attachments) as-is; it
does not read any of it from the sources. `MuxingApp`/`WritingApp` in the
output are `"mkvgo"`. The output also carries mkvmerge-style per-track
statistics tags (`BPS`, `DURATION`, `NUMBER_OF_FRAMES`, `NUMBER_OF_BYTES`)
accumulated during the stream — `WithBitrate()` reads them back head-only.

**Demux** -- extract tracks to raw streams:

```go
err := matroska.Demux(ctx, matroska.DemuxOptions{
    SourcePath: "video.mkv",
    OutputDir:  "./streams/",
    TrackIDs:   []uint64{1, 2},  // empty = all tracks
})
```

**Merge** -- combine all tracks from multiple MKVs:

```go
err := matroska.Merge(ctx, matroska.MergeOptions{
    OutputPath: "combined.mkv",
    Inputs: []matroska.MergeInput{
        {SourcePath: "video.mkv"},
        {SourcePath: "audio.mkv", TrackIDs: []uint64{1}},
    },
    Progress: func(processed, total int64) {
        fmt.Printf("%.1f%%\n", float64(processed)/float64(total)*100)
    },
})
```

Merge's metadata policy is first-wins: the output's title, chapters, tags and
attachments come from the first input only; the other inputs contribute
tracks, not metadata.

---

## Editing

### Full rewrite (edit + copy clusters)

```go
err := matroska.EditMetadata(ctx, "input.mkv", "output.mkv",
    func(c *matroska.Container) {
        c.Info.Title = "New Title"
        for i := range c.Tracks {
            if c.Tracks[i].Type == matroska.AudioTrack {
                c.Tracks[i].Language = "jpn"
            }
        }
    },
)
```

### In-place (instant, headers only)

Files written by mkvgo reserve `writer.MetadataReserve` bytes of Void after
the metadata, so in-place edits that grow it usually fit. The head SeekHead is
rebuilt (keeping its Cues entry) and a post-cluster Tags element (mux
statistics) is folded into the head without duplication.

Modifies the file directly without rewriting cluster data. Only safe for metadata changes that fit in the existing header space.

```go
err := matroska.EditInPlace(ctx, "video.mkv",
    func(c *matroska.Container) {
        c.Info.Title = "Quick Fix"
    },
)
```

### Attachments and chapters

```go
// Attach a file (ID auto-assigned); what to-mp4 carries as MP4 cover art
// when it is a cover.* image.
err := matroska.AddAttachment(ctx, "in.mkv", "out.mkv", matroska.Attachment{
    Name: "cover.jpg", MIMEType: "image/jpeg", Data: jpg,
})

// Remove by decimal ID or exact name; errors BEFORE writing when nothing matches.
err = matroska.RemoveAttachment(ctx, "in.mkv", "out.mkv", "cover.jpg")

// Replace the chapter list; ParseOGMChapters/FormatOGMChapters convert to and
// from the OGM text format (CHAPTER01=... / CHAPTER01NAME=...) that mkvmerge
// and ffmpeg understand.
chaps, err := matroska.ParseOGMChapters(file)
err = matroska.SetChapters(ctx, "in.mkv", "out.mkv", chaps)
```

### Add / Remove tracks

```go
// Remove tracks 3 and 4
err := matroska.RemoveTrack(ctx, "in.mkv", "out.mkv", []uint64{3, 4})

// Add a track from another file
err := matroska.AddTrack(ctx, "in.mkv", "out.mkv", matroska.TrackInput{
    SourcePath: "commentary.mkv",
    TrackID:    1,
    Language:   "eng",
    Name:       "Commentary",
})
```

---

## Reindex

`ops.Reindex` copies every cluster from `srcPath` to `dstPath` verbatim and writes a new SeekHead and Cues index derived from the cluster contents. All other elements (Info, Tracks, Tags, Chapters, Attachments) are also copied verbatim.

The result is always reopened afterwards and its Cues checked against the index built during the copy -- a light, head-only verification costing a few milliseconds. `Options.DeepVerify` additionally runs a full-read `Validate` on the output and a byte-level payload comparison (`CompareBlocks`) against the source, proving the copy verbatim rather than merely well-formed -- at the cost of a full read of the output (and of both files, for the comparison). A failed verification returns an error; `dstPath` is left as written.

```go
import "github.com/gravity-zero/mkvgo/mkv/ops"

err := ops.Reindex(ctx, "input.mkv", "output.mkv")
```

With a progress callback and the deep verification pass:

```go
err := ops.Reindex(ctx, "input.mkv", "output.mkv", mkv.Options{
    DeepVerify: true,
    Progress: func(processed, total int64) {
        fmt.Printf("\r%.1f%%", float64(processed)/float64(total)*100)
    },
})
```

### Resync (repair corrupted regions)

By default `Reindex` refuses a file whose top-level walk hits a corrupted region (an element whose declared size does not match its real extent, damaged bytes inside a cluster body, raw junk between clusters -- typical of old repacks and interrupted writes): the walker lands mid-payload and the garbage does not decode as an element. Such files often still play everywhere, because players resynchronize and carry on. `Options.Resync` opts the reindex into repairing them:

```go
err := ops.Reindex(ctx, "input.mkv", "output.mkv", mkv.Options{
    Resync: true,
    OnRepair: func(r mkv.RepairedRange) {
        fmt.Printf("reconstructed [%d,%d), %d bytes kept\n", r.StartOffset, r.EndOffset, r.BytesKept)
    },
    OnSkip: func(r mkv.DamagedRange) {
        fmt.Printf("dropped bytes [%d,%d), approx %dms-%dms\n",
            r.StartOffset, r.EndOffset, r.ApproxStartMs, r.ApproxEndMs)
    },
})
```

The repair is surgical. On a damaged cluster the walk re-derives the truth from the bytes: it parses cluster children from the body start ignoring the declared size, and on a break scans forward for the next chain-validated block (known child IDs, in-bounds sizes, track numbers from the file's real track set, timecode continuity across the gap), splitting the cluster around the damage instead of dropping its whole declared extent. A lying size field over an intact payload is corrected with zero media loss; damage inside a body loses only the unrecoverable bytes. Recovery never guesses timing: continuation runs are emitted under the original cluster's own Timestamp, and a region whose Timestamp cannot be read is not block-recovered at all. The SeekHead and Cues are rebuilt from what survives, and the usual verification chain still runs on the result.

Reconstructed regions are reported through `Options.OnRepair` and dropped ranges through `Options.OnSkip` (byte offsets plus the approximate presentation time lost), both called only after every check has passed. The repair is still refused when no valid resume point is found within the scan window (bounded, 64 MiB), when no cluster survives, or when more than half of the walked payload would be dropped: a mostly-damaged file must not silently "repair" into a stub.

`Options.CleanCut` additionally resumes video after each gap at the next video keyframe: the first recovered video frames after a gap are often P/B frames that reference lost pictures and decode with artifacts until the keyframe. Audio resumes immediately (its frames are independent). The dropped bytes are counted in the salvage report (`CleanCutBytes`) and the damaged range's end time extends to the resume keyframe.

A clean source produces output byte-identical to a strict `Reindex` and zero callback calls, so the option is free when no damage exists. `Options.Resync` applies to `Reindex` and `ReindexReplace`; `ReindexInPlace` refuses it (repairs cannot be patched into the file itself). Under `Resync`, `Options.DeepVerify`'s byte-comparison against the source only runs when nothing was skipped, repaired or clean-cut (a damaged source cannot be walked for a verbatim proof); the full-read `Validate` of the output always runs. For best-effort recovery of a file too damaged for these caps, see `Salvage` below; to see what a repair would do before running one, see `MapDamage`.

One honest limit: the recovery is structural, not decode-level. A block whose framing is intact but whose payload tail was overwritten is kept as-is (detecting it would require decoding the codec bitstream, which mkvgo never does) -- expect at worst one glitched frame at a damage boundary.

### ReindexReplace

`ops.ReindexReplace` rebuilds `path`'s seek index through a temporary copy in the same directory, runs the same verification chain as `Reindex` (light always, plus the deep pass when `Options.DeepVerify` is set), and only then atomically renames the copy over the original. The original is never touched until every check has passed. Needs write permission on the directory (temp file + rename), not just on the file itself.

```go
err := ops.ReindexReplace(ctx, "video.mkv")
```

`Options.KeepBackup` preserves the pre-op original as `path+".bak"` instead of discarding it:

```go
err := ops.ReindexReplace(ctx, "video.mkv", mkv.Options{KeepBackup: true, DeepVerify: true})
```

If a leftover `path+".mkvgo.tmp"` already exists (a previous run crashed mid-copy), `ReindexReplace` refuses to run rather than silently overwrite it -- remove the file first if it is safe to discard.

### ReindexInPlace

`ops.ReindexInPlace` rebuilds the seek index by patching the file itself: the new Cues element is appended inside the Segment, the Segment size extended, the head SeekHead repointed and any stale Cues element voided. Cluster bytes are never moved and no copy of the file is created, so the operation needs write access to the file only (not the directory) and uses no transient disk space beyond the new index itself.

It is crash-safe: every byte about to be overwritten is captured into a small journal appended inside the file (fsynced before any patch), the result is verified while the journal still allows a rollback -- the light head-only check always, plus the full-read `Validate` pass with `Options.DeepVerify` -- and the journal is truncated away only once the checks pass. Any failure (including a failed verification) restores the original bytes; a crash mid-operation is repaired by the automatic recovery the next run performs, or explicitly:

```go
err := ops.ReindexInPlace(ctx, "video.mkv", mkv.Options{DeepVerify: true})

// After a crash mid-operation: restore the original bytes without reindexing.
recovered, err := ops.RecoverInPlace(ctx, "video.mkv")
```

Once `ReindexInPlace` has returned successfully there is no undo -- the journal only exists during the operation. Streamed files (unknown-size clusters), truncated files and files whose head has no SeekHead or Void large enough for the rebuilt SeekHead are refused with an explicit error pointing at `Reindex`, which copies and can therefore rebuild anything readable.

Choosing a variant: `Reindex` never touches the source (safest, needs a second path), `ReindexReplace` swaps a verified copy over the original (atomic, needs directory permission and transient double disk), `ReindexInPlace` patches the file (file-only permission, no disk duplication, seconds instead of a full copy on large files).

### Salvage (damaged files)

`Reindex`/`Validate`/`BlockReader` refuse mid-file corruption by design -- that stays. `ops.Salvage` is the separate, explicitly lossy-tolerant operation: it walks `srcPath` exactly like `Reindex` (metadata elements and cluster payloads copied verbatim, the Cues index rebuilt from real video keyframes), but a structural failure inside the cluster stream -- a header that will not decode, a declared size that overflows, a cluster body whose children do not parse to its end -- is not fatal. The damaged region is repaired surgically when the bytes allow it (the same mechanism as `Reindex` with `Options.Resync` above: lying sizes corrected losslessly, chain-validated blocks around a gap kept, `Options.CleanCut` honored) and only what cannot be recovered is skipped and recorded. A truncated source yields a damaged range running to EOF (complete blocks before the cut are kept); a clean source yields zero damaged ranges and a result equivalent to `Reindex`.

```go
import "github.com/gravity-zero/mkvgo/mkv/ops"

report, err := ops.Salvage(ctx, "damaged.mkv", "recovered.mkv")
if err != nil {
    // A hard failure: the bounded resync scan gave up without reaching a
    // valid Cluster or real EOF, or a genuine I/O error. No output on failure.
    return err
}
for _, dr := range report.DamagedRanges {
    fmt.Printf("lost bytes [%d,%d), approx %dms-%dms\n", dr.StartOffset, dr.EndOffset, dr.ApproxStartMs, dr.ApproxEndMs)
}
fmt.Printf("%d clusters copied, %d bytes recovered, %d bytes skipped\n",
    report.ClustersCopied, report.BytesCopied, report.BytesSkipped)
```

`SalvageReport`:

| Field | Meaning |
| --- | --- |
| `ClustersCopied` | Number of clusters carried over into the output (including clusters rebuilt around damage). |
| `BytesCopied` | Total source bytes (metadata elements + cluster payloads) successfully carried over. |
| `BytesSkipped` | Total source bytes lost to damage (sum of every `DamagedRange`'s span). |
| `DamagedRanges` | `[]DamagedRange{StartOffset, EndOffset, ApproxStartMs, ApproxEndMs}`, one entry per skipped span, in file order. |
| `RepairedRanges` | `[]RepairedRange{StartOffset, EndOffset, BytesKept}`, one entry per region reconstructed around damage (media kept that a plain skip would have dropped). |
| `CleanCutBytes` | Video bytes intentionally dropped after gaps up to the next keyframe (`Options.CleanCut`). |

Never in-place: `dstPath` is always a separate file. The result is reopened afterwards and its Cues checked against the ones built during the walk -- the same light verification `Reindex` always runs -- so a bug in `Salvage` itself (not the source's damage) still surfaces as an error.

Prefer `Reindex` whenever the source is expected to be intact -- it fails loudly on any corruption instead of silently accepting data loss. `Reindex` with `Options.Resync` covers the middle ground: tolerant like `Salvage`, but capped (refuses a mostly-damaged source) and running the full reindex verification chain. Reach for `Salvage` only when even that refuses and the goal is the best possible recovery, not proof of fidelity.

### MapDamage (dry-run)

`ops.MapDamage` runs the exact walk `Salvage` runs - surgical recovery, damage ranges, repairs, optional clean-cut accounting - but writes nothing. The returned report is the one the equivalent `Salvage` (or `Reindex` with `Options.Resync`) would produce, so the decision to repair can be made with the numbers in hand:

```go
report, err := ops.MapDamage(ctx, "suspect.mkv")
if err != nil {
    return err
}
for _, dr := range report.DamagedRanges {
    fmt.Printf("a repair would lose [%d,%d), approx %dms-%dms\n",
        dr.StartOffset, dr.EndOffset, dr.ApproxStartMs, dr.ApproxEndMs)
}
```

The facade re-exports the types and functions: `matroska.Salvage`, `matroska.MapDamage`, `matroska.SalvageReport`, `matroska.DamagedRange`, `matroska.RepairedRange`.

### Rollback delta (undo a repair without a backup copy)

Every repair that rewrites a file (`Reindex` strict or with `Resync`, `ReindexReplace`, `Salvage`, `ReindexInPlace`) can emit the inverse delta - the recipe to reconstruct the pre-repair ORIGINAL from the repaired output - instead of forcing the caller to keep a full backup copy. The repair copies cluster payloads verbatim and already knows where each run lands, so the delta is mostly "copy this range of the repaired file" plus literals for what was dropped or re-encoded: typically well under 0.1% of the source size. Emitting it costs one extra sequential read of the repaired file (its sha256 gates the rollback); the in-place path persists its crash journal as the entry and emits it while the journal still allows undoing a failure.

```go
delta, _ := os.Create("movie.rbd")
defer delta.Close()

err := ops.Reindex(ctx, "movie.mkv", "repaired.mkv", mkv.Options{
    Resync:           true,
    RollbackSink:     delta,
    RollbackRequired: true, // a failed delta fails the repair; default false = best-effort
    OnRollback: func(i mkv.RollbackInfo) {
        fmt.Printf("delta: %d bytes, original sha256 %x\n", i.Bytes, i.SrcSHA256)
    },
})

// Later, to undo the repair:
d, _ := os.Open("movie.rbd")
defer d.Close()
err = ops.ApplyRollback(ctx, "repaired.mkv", d, "restored.mkv")
```

`ApplyRollback` is hash-gated twice: it refuses to reconstruct when the repaired file no longer matches the entry (it changed since the repair), and it never delivers an output whose sha256 does not match the original's. A torn or corrupted entry (per-entry crc32c) is refused, and no output file survives a refusal. The entry format is framed and append-friendly, so a caller can pack many entries into one ledger file and hand `ApplyRollback` a reader positioned at the right one. `MapDamage` ignores the sink (a dry-run writes nothing, so there is nothing to roll back); a remux-class operation (re-laced payloads, no byte mapping) never emits a delta - the absence of an entry tells the caller to fall back to a full copy.

The facade re-exports `matroska.ApplyRollback` and `matroska.RollbackInfo`.

---

## Analyze (Stream Statistics)

`ops.Analyze` walks a Matroska/WebM file's block HEADERS - track, timecode, keyframe flag, byte size, duration - to compute the frame-accurate stream statistics a `-count_frames`/`-show_packets`-style probe reports, without ever decoding a sample: no pixel or audio decoding happens anywhere in the walk.

The walk is head-only: `BlockReader.SetHeaderOnly` discards each unlaced block's payload instead of reading it (`Block.Size` is still reported), so the cost is proportional to the block-HEADER count, never the media volume - a two-hour 4K file and a two-hour 480p file with the same frame count cost the same to analyze. A laced block's lacing header still has to be decoded to size its frames, but the payload is dropped right after, never held. Memory stays bounded: only small per-track counters and a trailing-1-second bitrate window are kept, never the blocks themselves - `Analyze` scales to files with millions of blocks.

```go
import "github.com/gravity-zero/mkvgo/mkv/ops"

report, err := ops.Analyze(ctx, "video.mkv")
if err != nil { return err }

fmt.Printf("duration %dms (declared %dms), %d clusters, %d blocks\n",
    report.DurationMs, report.DeclaredDurationMs, report.ClusterCount, report.BlockCount)

for _, ts := range report.Tracks {
    fmt.Printf("track %d (%s/%s): %d frames (%d packets), %d keyframes, avg %d bps, peak %d bps\n",
        ts.TrackID, ts.Type, ts.Codec, ts.Frames, ts.Packets, ts.Keyframes, ts.AvgBitrateBps, ts.PeakBitrateBps)
}
for _, w := range report.Warnings {
    fmt.Println("warning:", w)
}
```

`TrackStats` (one per track):

| Field | Meaning |
| --- | --- |
| `Frames` | Exact frame count, lacing expanded: a laced audio block stores several frames under one stored (Simple)Block, and each is counted individually. |
| `Packets` | Count of STORED (Simple)Block/BlockGroup elements - what a laced track's `Frames` divides among. `Packets == Frames` for an unlaced track (real-world video is never laced). |
| `Keyframes` | Count of frames carrying the keyframe flag. |
| `Bytes` | Sum of frame payload sizes seen (`Block.Size`, populated even in the header-only walk). |
| `DurationMs` | This track's last frame's end time (timecode + duration), the maximum seen over every frame. |
| `AvgBitrateBps` / `PeakBitrateBps` | Average over the whole track; peak over the densest trailing 1-second window of frame bytes. |
| `MinGopFrames` / `MaxGopFrames` / `AvgGopFrames` | Frame-count spans between consecutive VIDEO keyframes. Zero for non-video tracks - audio has no GOP structure, every frame is independently decodable. |
| `KeyframeEveryMsAvg` | Average milliseconds between consecutive video keyframes. |
| `Reordered` | Video-only, a decode-free HEURISTIC: a presentation timecode (`Block.Timecode`) that goes backwards in decode order (the order `Next` delivers blocks) is consistent with B-frame reordering, but is not a certainty from headers alone - treat it as a hint, not a decoded fact. |
| `FrameRateAvg` | `Frames*1000/DurationMs`. |
| `FrameRateMode` | Video-only: `"cfr"` (constant frame rate) or `"vfr"` (variable), derived decode-free from consecutive frame-timecode deltas (min/max spread within +-1ms counts as CFR - Matroska timecodes are millisecond-scale, so exact-equal deltas would be too strict). `""` when unknown (fewer than 2 frames) or for a non-video track. |
| `FrameDurationVarianceNs` | The spread (max delta - min delta) between consecutive video frame timecodes, in nanoseconds - 0 for a perfect CFR track, a diagnostic magnitude for VFR. |

A VFR video track (`FrameRateMode == "vfr"`) adds a Warning: some downstream pipelines (fixed-segment HLS, certain hardware decoders) assume constant frame rate and can misbehave on true VFR content.

`AnalyzeReport`:

| Field | Meaning |
| --- | --- |
| `DurationMs` | The container's TRUE duration - the latest track end seen during the walk. |
| `DeclaredDurationMs` | The Segment `Info` `Duration` element, before the walk confirms it. |
| `OverallBitrateBps` | Total bytes across every track, over `DurationMs`. |
| `ClusterCount` / `BlockCount` | Number of Cluster elements entered, and stored (Simple)Block/BlockGroup elements seen. |
| `Tracks` | `[]TrackStats`, one per track, in `Container.Tracks` order. |
| `Warnings` | Timing sanity issues: a declared-vs-true duration mismatch over 1 second, a track's timecode jumping backwards by more than a second, a track with zero frames, or a track whose frame durations could not be determined (no `BlockDuration`, no usable `DefaultDuration`). |

Supports the FS port like every other operation (`mkv.Options{FS: ...}`), so a remote file can be analyzed the same way `Validate` and `Reindex` do.

The facade re-exports it: `matroska.Analyze`, `matroska.AnalyzeReport`, `matroska.TrackStats`.

MP4 support is a follow-up: an MP4's sample table (`stsz`/`stss`/`stts`) already carries most of the per-sample data `Analyze` needs, but wiring it through the same report shape is not done yet.

---

## Fingerprint (Content Identity / Dedup)

`ops.Fingerprint` computes a container-independent content identity for a file: a per-track payload SHA-256 (the same digest `CompareBlocks` uses to prove a round-trip byte-identical), plus a single `Presentation` hash over all of them. Two files carrying the same audio/video/subtitle streams fingerprint identically even when their container metadata differs (title, muxing app), their tracks are stored in a different order, or one is Matroska/WebM and the other MP4/MOV - the use case is cross-container dedup in a media library: detect that a re-encode-free remux, or a re-mux with reordered tracks or a different container, is really the same content, without a byte-for-byte comparison of the containers themselves.

MP4/MOV sources are hashed by remuxing to a temporary Matroska file first (`RemuxFromMP4` copies every audio/video sample's compressed bytes verbatim), then running the exact same digest engine on that file - so an MP4 and an MKV carrying the same encode fingerprint identically. The one normalization: subtitle tracks are decoded to plain UTF-8 text (tx3g/WebVTT) rather than digesting the raw MP4 sample framing, so a subtitle track's digest reflects its decoded text, not its MP4 container bytes. A track in a codec `RemuxFromMP4` cannot carry is absent from the report.

Unlike `Analyze`, this is a FULL read: every track's frame payload is read and hashed, so the cost is proportional to the media volume, not just the block-header count.

```go
import "github.com/gravity-zero/mkvgo/mkv/ops"

fp, err := ops.Fingerprint(ctx, "video.mkv")
if err != nil { return err }

fmt.Println("presentation:", fp.Presentation)
for _, tf := range fp.Tracks {
    fmt.Printf("track %d (%s/%s): %s\n", tf.TrackID, tf.Type, tf.Codec, tf.SHA256)
}
```

`FingerprintReport`:

| Field | Meaning |
| --- | --- |
| `Presentation` | Hex SHA-256 identity for the whole file's content - see the recipe below. |
| `Tracks` | `[]TrackFingerprint`, one per track, in `Container.Tracks` order. |

`TrackFingerprint`:

| Field | Meaning |
| --- | --- |
| `TrackID` | The track's ID (matches `Track.ID`, `TrackStats.TrackID`). |
| `Type` / `Codec` | The track's type and codec string. |
| `SHA256` | Hex SHA-256 over the track's frame payloads in decode order - the exact digest `CompareBlocks` computes to prove a lossless round-trip. |

**Presentation recipe** (reproducible independently of this implementation):

1. For each track, take `hex(SHA-256 over its frame payloads in decode order)` - `TrackFingerprint.SHA256`.
2. Build a sort key `"type|codec|sha256hex"` for each track and sort the tracks by that key, ascending, byte-wise. Sorting by content (not by `TrackID` or file order) means a remux that reorders tracks produces the same sorted sequence.
3. Concatenate the sorted tracks' raw 32-byte SHA-256 sums (not their hex form) in that order, and take the SHA-256 of the concatenation.
4. `Presentation` is that final hash, hex-encoded.

Supports the FS port like every other operation (`mkv.Options{FS: ...}`).

The facade re-exports it: `matroska.Fingerprint`, `matroska.FingerprintReport`, `matroska.TrackFingerprint`.

---

## Stream I/O (non-seekable io.Reader / io.Writer)

### Reading from a stream

`reader.ReadStream` parses the EBML header and front-loaded segment metadata from a plain `io.Reader`, then returns a `*BlockReader` ready to iterate blocks. No Seek is ever issued. SeekHead and Cues are skipped because they are not usable in a forward-only stream.

```go
import "github.com/gravity-zero/mkvgo/mkv/reader"

c, br, err := reader.ReadStream(ctx, r) // r is any io.Reader
if err != nil { return err }

fmt.Println(c.Info.Title)
for _, t := range c.Tracks {
    fmt.Printf("#%d %s %s\n", t.ID, t.Type, t.Codec)
}

for {
    block, err := br.Next()
    if err == io.EOF { break }
    if err != nil { return err }
    // block.TrackNumber, block.Timecode, block.Keyframe, block.Data
}
```

`reader.NewStreamBlockReader` provides a lower-level entry point when the caller has already parsed metadata elsewhere and only needs block iteration from a bare `io.Reader`:

```go
br, err := reader.NewStreamBlockReader(r, timecodeScale)
```

### Writing to a stream

`writer.NewStreamWriter` writes a valid MKV stream to any `io.Writer` without ever seeking back. The Segment and each Cluster are written with unknown size. No SeekHead or Cues are emitted.

```go
import "github.com/gravity-zero/mkvgo/mkv/writer"

info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
sw, err := writer.NewStreamWriter(w, info, tracks) // w is any io.Writer
if err != nil { return err }
defer sw.Close()

// A new cluster opens automatically on the first keyframe block.
if err := sw.WriteBlock(mkv.Block{
    TrackNumber: 1,
    Timecode:    0,
    Keyframe:    true,
    Data:        frameData,
}); err != nil {
    return err
}

// Force a cluster boundary at any point.
sw.FlushCluster()
```

---

## Splitting and Joining

**Split by time ranges:**
```go
files, err := matroska.Split(ctx, matroska.SplitOptions{
    SourcePath: "video.mkv",
    OutputDir:  "./parts/",
    Ranges: []matroska.TimeRange{
        {StartMs: 0, EndMs: 300000},
        {StartMs: 300000, EndMs: 0},  // 0 = end of file
    },
})
// files = ["./parts/video_001.mkv", "./parts/video_002.mkv"]
```

**Split into fixed-duration, keyframe-aligned segments:**
```go
files, err := matroska.Split(ctx, matroska.SplitOptions{
    SourcePath: "video.mkv",
    OutputDir:  "./segments/",
    EveryMs:    6 * 60 * 1000, // ~6-minute parts, cut at video keyframes
})
```

Boundaries come from the Cues index (the first keyframe at/after each
multiple); a file without Cues must be reindexed first. `Pattern` names the
parts (default `part_%03d.mkv`); when splitting by chapters the `{title}`
token is replaced by the sanitized chapter title.

**Split by chapters:**
```go
files, err := matroska.Split(ctx, matroska.SplitOptions{
    SourcePath: "video.mkv",
    OutputDir:  "./chapters/",
    ByChapters: true,
})
```

Cut policy (keyframe alignment): a segment starts at the first **video
keyframe** at/after `StartMs` (leading audio and mid-GOP video are dropped so
the segment starts decodable) and ends right before the next video keyframe
at/after `EndMs` (the straddling GOP is kept, so chaining segments loses no
frame). A range that contains media but no video keyframe is an explicit
error. Audio-only sources cut exactly at the requested times. Chapters are
clipped to each segment's range and rebased to its timeline; block timecodes
are rebased to start at 0.

**Join sequential files:**
```go
err := matroska.Join(ctx, []string{"part1.mkv", "part2.mkv"}, "full.mkv")
```

Join requires the same track layout (count, types, codecs, codec
configuration) in every file and errors on mismatch. The output's
title/chapters/tags/attachments come from the first file (first-wins);
chapters keep their original timestamps.

---

## Self-verifying files (content hashes)

```go
// Store each track's content SHA-256 as a CONTENT_SHA256 tag (dstPath "" =
// in place, instant on mkvgo-written files thanks to the metadata reserve).
err := matroska.WriteContentHashes(ctx, "archive.mkv", "")

// Later — detect bit rot / transfer corruption, no external checksum file:
mismatches, err := matroska.VerifyContentHashes(ctx, "archive.mkv")
// nil mismatches = every hashed track is byte-intact.

// MP4: hashes are stored at remux time (freeform ilst atoms), verified the
// same way — mkvgo does not rewrite MP4 metadata in place.
err = mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4", mp4.Options{ContentHashes: true})
mm, err := mp4.VerifyContentHashes(ctx, "out.mp4")
```

---

## Custom Filesystem (FS Port)

The `mkv.FS` struct lets you swap out OS file operations. Pass it via `Options` to any operation. When `nil`, operations use the real filesystem.

```go
import "github.com/gravity-zero/mkvgo/mkv"

s3fs := &mkv.FS{
    Open: func(path string) (mkv.ReadSeekCloser, error) {
        // Implement S3 GetObject with seeking
        return myS3Reader(path)
    },
    Create: func(path string) (mkv.WriteSeekCloser, error) {
        // Implement S3 multipart upload
        return myS3Writer(path)
    },
    Stat: func(path string) (os.FileInfo, error) {
        return myS3Stat(path)
    },
    MkdirAll: func(path string, perm os.FileMode) error {
        return nil // S3 doesn't need directories
    },
}

// Use it with any operation
err := matroska.EditMetadata(ctx, "s3://bucket/in.mkv", "s3://bucket/out.mkv",
    func(c *matroska.Container) { c.Info.Title = "Updated" },
    matroska.Options{FS: s3fs},
)
```

FS methods and their OS fallbacks:

| Method | Fallback | Used for |
|---|---|---|
| `Open` | `os.Open` | Reading source files |
| `Create` | `os.Create` | Writing output files |
| `OpenFile` | `os.OpenFile` | In-place editing |
| `Stat` | `os.Stat` | File size for progress |
| `MkdirAll` | `os.MkdirAll` | Creating output directories |
| `WriteFile` | `os.WriteFile` | Writing small files (attachments) |
| `Remove` | `os.Remove` | Cleanup on error |

### In-memory FS (`mkv.MemFS`)

A ready-made implementation covering the whole port: every operation runs on
byte slices with no filesystem at all — the WebAssembly build's foundation
([docs/wasm.md](wasm.md)), and handy in tests or servers that assemble outputs
to ship elsewhere.

```go
m := mkv.NewMemFS()
m.Put("in.mkv", srcBytes)

err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4", mp4.Options{FS: m.FS(), FastStart: true})
mp4Bytes := m.Get("out.mp4")

// Multi-file outputs land in the same map:
err = mp4.RemuxToHLS(ctx, "in.mkv", "hls", mp4.Options{FS: m.FS()})
for _, p := range m.Paths() { … }   // hls/master.m3u8, hls/init.mp4, hls/seg00001.m4s, …
```

### Remote files over HTTP Range (`httpfs`)

`github.com/gravity-zero/mkvgo/httpfs` implements the port over HTTP(S) Range
requests (a separate package, so builds that don't need it — the wasm binary —
don't link `net/http`). Combined with the head-only probe, indexing a media
library on S3/HTTP transfers a few ranged kilobytes per file:

```go
import "github.com/gravity-zero/mkvgo/httpfs"

f := httpfs.New()                       // Options: Client, WindowSize, Header (auth)
c, err := matroska.OpenMetaWithFS(ctx, "https://nas/movie.mkv", f.Port())
fmt.Println(c.Tracks, f.BytesFetched()) // ~a window or two, whatever the file size
```

Reads are cached in 512 KiB windows (configurable). The server must answer
`206 Partial Content`; one that ignores `Range` gets an explicit error rather
than a silent full download. The FS is read-only — for a remux whose source is
remote and destination local, `httpfs.Hybrid()` routes URLs to HTTP and
everything else (including writes) to the OS:

```go
err := mp4.RemuxToMP4(ctx, "https://nas/movie.mkv", "out.mp4",
    mp4.Options{FS: httpfs.Hybrid(), FastStart: true})   // a streamed download
```

### Remote files over S3 (`s3fs`)

`github.com/gravity-zero/mkvgo/s3fs` is a read-only `mkv.FS` port for S3 (or an S3-compatible service): it reuses `httpfs` for the actual Range/window mechanics, wiring an `*http.Client` whose `http.RoundTripper` signs every request with AWS Signature Version 4 -- `crypto/hmac` + `crypto/sha256` only, no AWS SDK dependency.

```go
import "github.com/gravity-zero/mkvgo/s3fs"

fs := s3fs.New(s3fs.Options{Region: "us-east-1"}) // creds/region/endpoint fall back to the AWS env
c, err := matroska.OpenMetaWithFS(ctx, "s3://my-bucket/movies/one.mkv", fs.Port())
fmt.Println(c.Tracks, fs.BytesFetched()) // a window or two, whatever the file size
```

`s3fs.Options`:

| Field | Meaning |
| --- | --- |
| `Region` | AWS region. Falls back to `AWS_REGION`, then `AWS_DEFAULT_REGION`, then `"us-east-1"`. |
| `Endpoint` | Overrides the S3 host (S3-compatible services). Empty means `https://s3.<region>.amazonaws.com`. Falls back to `AWS_ENDPOINT_URL`. May include a scheme; a bare `host:port` is treated as `https`. |
| `AccessKey`, `SecretKey`, `SessionToken` | SigV4 credentials. Fall back to `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`. |
| `PathStyle` | `https://<host>/<bucket>/<key>` instead of the default virtual-hosted `https://<bucket>.<host>/<key>`. Needed by some S3-compatible services and by bucket names containing dots. |
| `WindowSize` | Ranged-fetch granularity in bytes, passed through to `httpfs.Options.WindowSize`. |

Signing covers GET/HEAD with an unsigned payload (`x-amz-content-sha256: UNSIGNED-PAYLOAD` -- this package never needs to sign a request body, only Range reads), the host, date, session token (when present) and `Range` header, with the S3 URI-encoding rule for the key (each `/`-separated segment percent-encoded, slashes kept literal). The FS is read-only, like `httpfs`.

---

## HTTP serving (`mkvhttp`)

`github.com/gravity-zero/mkvgo/mkvhttp` is a drop-in `http.Handler` for the on-demand plans (`mp4.PlanHLS`, `mp4.PlanABR`): nothing is pre-generated on disk, every resource a player requests is built the first time it is asked for, and static-VOD HTTP semantics come for free.

```go
import "github.com/gravity-zero/mkvgo/mkvhttp"

plan, err := mp4.PlanHLS(ctx, "movie.mkv", mp4.Options{})
http.Handle("/hls/", http.StripPrefix("/hls/", mkvhttp.Handler(plan)))
http.ListenAndServe(":8478", nil)
```

`mp4.HLSPlan` and `mp4.ABRPlan` already satisfy `mkvhttp.Resolver` as-is -- both declare `func (p *T) Resource(ctx context.Context, name string) ([]byte, string, error)`, matching the interface exactly -- so `Handler(plan)` works directly with either; no adapter is needed. A `Resolver` backed by anything else can be written by hand, or wrapped with `mkvhttp.ResolverFunc` for a plain function.

Semantics:

| Aspect | Behaviour |
| --- | --- |
| Methods | `GET`/`HEAD` only; `405` (with `Allow`) otherwise. `OPTIONS` gets a `204` CORS preflight response when `Options.AllowCORS` is set. |
| Resource name | The request path with its leading slash trimmed. Mount under a prefix with `http.StripPrefix`. |
| ETag | Strong: the SHA-256 of the resource's bytes, quoted. Deterministic -- two independent handlers over the same plan produce the identical ETag. |
| Conditional GET | A matching `If-None-Match` gets a bare `304`. |
| Content-Type | From the `Resolver`, set on the response before `http.ServeContent` runs, so its own name-extension sniffing never overrides it. |
| Range | Served by `http.ServeContent` over a `bytes.Reader` (no modtime -- the ETag already identifies the exact bytes). |
| Cache-Control | `.m3u8`/`.mpd` get `no-cache` (their bytes name segments that can be re-derived); every other resource gets `public, max-age=31536000, immutable` -- safe because a segment/init name always maps to the same bytes for a given source (see "Deterministic outputs" below). |
| Errors | A `Resolver` error that is (or wraps, via `%w`) `mkvhttp.ErrNotFound` answers `404`; any other error answers `502` with a terse body. |
| CORS | `Options{AllowCORS: true}` adds `Access-Control-Allow-Origin: *` and exposes the headers a player needs (`Content-Length`, `Content-Range`, `ETag`, `Accept-Ranges`), plus the `OPTIONS` preflight response above. |

The CLI's `mkvgo serve` (see `docs/cli.md`) is this package wired to `mp4.PlanHLS` with a graceful-shutdown `net/http.Server`.

### Direct-play (`mkvhttp.FileHandler`)

`mkvhttp.FileHandler` serves one local file as-is, for a client that can direct-play it (no packaging, no decode/transcode) -- the counterpart to `Handler`'s on-demand plans:

```go
http.Handle("/play/movie.mkv", mkvhttp.FileHandler("movie.mkv"))
```

It streams straight from an `*os.File` via `http.ServeContent` -- the file is never read into memory.

| Aspect | Behaviour |
| --- | --- |
| Methods | `GET`/`HEAD` only; `405` (with `Allow`) otherwise. `OPTIONS` gets a `204` CORS preflight response when `Options.AllowCORS` is set. |
| Range | Full support (seeking/scrubbing), served by `http.ServeContent` directly over the open file. |
| ETag | Strong, but O(1) in file size: the SHA-256 of `"<size>-<mtime-unix-nano>"` from a single `os.Stat`, never the file's content -- hashing a multi-gigabyte source on every request would defeat the point of a fast direct-play handler. |
| Conditional GET | A matching `If-None-Match` gets a bare `304`. |
| Content-Type | From the extension, set before `http.ServeContent` runs so its own sniffing never overrides it: `.mkv` &rarr; `video/x-matroska`, `.webm` &rarr; `video/webm`, `.mp4`/`.m4v` &rarr; `video/mp4`, else `application/octet-stream`. |
| Cache-Control | `public, max-age=31536000, immutable` -- the bytes at a given path never change. |
| Errors | Missing file answers `404`; any other open/stat error answers `500`. |
| CORS | Same as `Handler`: `Options{AllowCORS: true}`. |

The CLI's `mkvgo serve --direct`/`--auto` (see `docs/cli.md`) wires this to a graceful-shutdown `net/http.Server`.

---

## Deterministic outputs

mkvgo's writers never stamp wall-clock times or random IDs: `MuxingApp`/
`WritingApp` default to the fixed string `"mkvgo"`, `DateUTC` is only ever
copied from the source, MP4 `creation_time`/`modification_time` are written as
zero, and element order is fixed. **The same input and options produce
byte-identical output**, across runs and machines — verified by a regression
test over the in-memory FS (MKV rewrite, MP4 remux, HLS segments). This makes
outputs safe for content-addressed storage and dedup (the file hash is a
stable key) and for golden-file tests.

---

## Progress Callbacks

Long-running operations accept a `ProgressFunc` via `Options`:

```go
opts := matroska.Options{
    Progress: func(processed, total int64) {
        if total > 0 {
            fmt.Printf("\r%.1f%%", float64(processed)/float64(total)*100)
        }
    },
}

err := matroska.RemoveTrack(ctx, "in.mkv", "out.mkv", []uint64{3}, opts)
```

`total` is `-1` when the file size is unknown.

Progress is honoured by every cluster-walking operation: Mux, Merge, Split,
Join, Demux, AddTrack, RemoveTrack, EditMetadata, Reindex, RemuxToWebM and the
MP4 remuxes (`mp4.Options.Progress`). Multi-source operations (Mux/Merge/Join)
aggregate: `processed`/`total` cover the byte total of all inputs.

---

## Subtitle Parsing

### SRT

```go
import "github.com/gravity-zero/mkvgo/mkv/subtitle"

entries, err := subtitle.ParseSRT("subs.srt")
// []subtitle.SRTEntry{StartMs, EndMs, Text}
```

### ASS/SSA

```go
assFile, err := subtitle.ParseASS("subs.ass")
// assFile.Header (raw [Script Info] + [V4+ Styles] block), assFile.Events
// Each subtitle.ASSEvent{StartMs, EndMs, Fields}
```

### Extract from MKV

```go
// As SRT
err := matroska.ExtractSubtitle(ctx, "video.mkv", trackID, "out.srt")

// As ASS
err := matroska.ExtractASS(ctx, "video.mkv", trackID, "out.ass")
```

### Merge into MKV

```go
// SRT
err := matroska.MergeSubtitle(ctx, "video.mkv", "subs.srt", "out.mkv", "eng", "English")

// ASS
err := matroska.MergeASS(ctx, "video.mkv", "subs.ass", "out.mkv", "jpn", "Japanese")
```

---

## Playability and ABR Ladder

### Playability (`matroska.Playability`)

A decision over head-only metadata: whether a file direct-plays on a given
target, needs only a container remux, or needs a transcode - without probing
an external tool and without decoding. It reads the same head-only metadata
`probe` prints (codec, profile, level, pixel format, bit depth, resolution,
colour/HDR/Dolby Vision, audio channels/sample rate); no block walk.

```go
target, _ := matroska.TargetByName("safari")
report, err := matroska.Playability(ctx, "movie.mkv", target)
// report.OverallVerdict: "direct-play" | "remux" | "transcode"
// report.RemuxContainer: set when OverallVerdict is "remux" (e.g. "mp4")
// report.Tracks[i]: TrackID, Type, Verdict, Reasons (why remux/transcode)
```

Per track: the codec/profile/level/resolution/bit-depth/HDR are checked
against the target first - any unsupported one is a hard "transcode" (with
the specific reason, e.g. `level 5.1 exceeds target max 4.1`). Only when the
codec itself is fine does the source **container** matter: already accepted
by the target -> "direct-play"; carried by a different container the target
also accepts -> "remux" (`RemuxContainer` names the cheapest one - mkvgo can
do that remux without a transcode); carried by none -> "transcode". The
overall verdict is the worst of every track's verdict. A track whose codec
level is absent from the source metadata (e.g. `Track.Level == nil`) is
treated conservatively as unsupported rather than guessed, and the reason
says so.

`Target` is a plain, overridable capability table:

```go
type Target struct {
    Name        string
    Container   []string // cheapest first: "mp4", "webm", "hls", "dash", "mkv" (rare)
    VideoCodecs []string
    AudioCodecs []string
    MaxWidth, MaxHeight         int
    MaxLevelH264, MaxLevelHEVC  int // Track.Level encoding: H.264 10x, HEVC 30x; 0 = no limit
    HDR, DolbyVision            bool
    HEVCMain10, VP9Profile2     bool // 10-bit support
}
```

`TargetByName` returns a built-in, reviewable profile - a plain data table, no
logic - for `"safari"`, `"chrome"`, `"firefox"`, `"chromecast-gen3"`,
`"mse-generic"`, `"chromium-generic"`, `"brave"`, `"opera"`, `"vivaldi"`,
`"samsung-internet"` and `"edge"`. It is deliberately conservative wherever
real-world support is hardware/OS-dependent: HEVC is unsupported by default on
every Chromium-family target except Edge (which decodes it through the
Windows HEVC Video Extension); Safari's VP9/AV1 support is left out for the
same reason. Brave/Opera/Vivaldi/Samsung Internet share Chrome's table
unchanged (`chrome == chromium-generic` in every field but `Name`). A caller
who knows better - a newer OS release, a device with hardware HEVC decoding -
builds its own `Target` instead; see `mkv/ops/targets.go` for the full,
line-commented baseline table and its stated assumptions.

### ABR ladder (`matroska.RecommendLadder` / `RecommendLadderFor`)

A deterministic, capped rung recommendation - guidance for an external
encoder, not a guarantee (mkvgo never transcodes):

```go
rungs, err := matroska.RecommendLadderFor(ctx, "movie.mkv")
// []matroska.Rung{Width, Height, BitrateKbps, Label}, tallest first

rungs = matroska.RecommendLadder(matroska.LadderInput{
    SourceWidth: 1920, SourceHeight: 1080, SourceBitrateKbps: 6000,
    Codec: "h264", FrameRate: 24,
})
```

Rungs come from a standard H.264-baseline ladder (2160p/1080p/720p/480p/360p,
each with an editorial bitrate) filtered and scaled:

- **never upscale**: a rung taller or wider than the source is dropped;
- **never exceed the source bitrate**: every rung's bitrate is capped at
  `SourceBitrateKbps` when known (the cap is applied uniformly across rungs,
  so the ladder stays monotonically non-decreasing with resolution even when
  the cap clips several rungs to the same value);
- **codec efficiency**: the baseline bitrate is scaled by a documented,
  approximate multiplier for codecs more efficient than H.264 at the same
  quality (HEVC 0.6x, AV1 0.5x, VP9 0.65x; anything else, including an unknown
  codec, gets 1.0x - no assumed gain);
  - content above 30fps gets an additional 1.3x bump (more motion to encode
    per second for the same quality);
- when the source is shorter than every standard rung, a single rung at the
  source's own resolution is returned instead of an empty ladder.

`RecommendLadderFor` derives `LadderInput` from the file's video track
(head-only metadata, Matroska/WebM or MP4/MOV) - width/height, `Track.Bitrate`
when present, codec, and frame rate.

---

## Ingest (One-Call Serving Plan)

`matroska.Ingest` composes `Playability`, `RecommendLadderFor` and
`ReindexInPlace` into a single decision for a media server's per-file
onboarding, so the caller does not hand-roll "check playability, then check
the seek index, then maybe reindex, then maybe recommend a ladder" for every
file. Like everything else in this package: no decode, no transcode - a
"transcode" verdict only returns a recommended ladder for an external encoder
to act on.

```go
plan, err := matroska.Ingest(ctx, "movie.mkv", matroska.IngestOptions{
    Target:  "safari",       // default "mse-generic" when empty
    Reindex: true,           // patch in a seek index if a remux decision needs one and none exists
})
if err != nil { return err }

switch plan.Strategy {
case matroska.StrategyDirectPlay:
    // serve the source file as-is (byte-range)
case matroska.StrategyRemuxHLS:
    // package on-demand HLS/CMAF from the source; plan.RemuxContainer names the container
case matroska.StrategyTranscode:
    // plan.Ladder holds the recommended rungs for an external encoder
}
```

`ServingPlan` fields:

- `Strategy`: `"direct-play"` | `"remux-hls"` | `"transcode"` (the constants
  `StrategyDirectPlay`/`StrategyRemuxHLS`/`StrategyTranscode`).
- `Target`, `SourceContainer` (`"mkv"` or `"mp4"`, as mkvgo's own head-only
  sniffing resolves it - see `Playability`), `RemuxContainer` (set only for
  `remux-hls`).
- `HasSeekIndex`, `NeedsReindex`: whether the source already carries a
  head-discoverable Cues index (`reader.WithCues`), checked only when the
  strategy is `remux-hls` (on-demand HLS needs to seek into the source).
- `Reindexed`: true when `Ingest` itself performed an in-place reindex during
  this call (`opts.Reindex` was set and it succeeded).
- `ReindexInPlacePossible`: false means the file's layout cannot hold a
  head-discoverable seek index in place (see `ErrIndexNotHeadDiscoverable`) -
  the caller falls back to a copy reindex (`Reindex`/`ReindexReplace`).
  `Ingest` does not fail in that case: the plan is still returned, with a
  `Reasons` entry pointing at the fallback.
- `Playability`: the full per-track report.
- `Analysis`: populated only when `IngestOptions.IncludeAnalysis` is set.
- `Ladder`: populated only when `Strategy` is `transcode`.
- `Reasons`: a short, human-readable decision trail - one line per decision
  `Ingest` made, in order (target/container resolved, why remux or transcode,
  seek-index status, reindex outcome).

`IngestOptions` embeds `Options` (the `FS`/`Progress` port), plus `Target`,
`IncludeAnalysis` and `Reindex`.

---

## Error Handling

All functions return `error`. No panics, no logging.

The reader tolerates corrupted bodies: a zeroed or padded region between clusters (seen in some real-world rips) does not abort the read -- the parser resyncs to the next valid Cluster and returns the metadata gathered so far. A file truncated mid-element after the head metadata likewise returns what was parsed (Info + Tracks) rather than failing. A damaged EBML/Segment header still returns an error. Malformed input never panics.

```go
c, err := matroska.Open(ctx, path)
if err != nil {
    // A misnamed .mkv that is actually an MP4-family file reports a typed error,
    // so a caller dispatching by extension can re-route to the mp4 reader.
    if errors.Is(err, matroska.ErrNotMatroska) {
        return mp4.OpenMeta(ctx, path)
    }
    // Could be: file not found, invalid EBML, truncated file, etc.
    return fmt.Errorf("open %s: %w", path, err)
}
```

Validation returns structured issues instead of failing:

```go
issues, err := matroska.Validate(ctx, "video.mkv")
if err != nil { return err }

for _, iss := range issues {
    // iss.Severity: "error" or "warning"
    // iss.Message: human-readable description
    fmt.Println(iss)
}
```

Comparison returns structured diffs:

```go
diffs, err := matroska.Compare(ctx, "a.mkv", "b.mkv")
for _, d := range diffs {
    // d.Type: "added", "removed", "changed"
    // d.Section, d.Detail
    fmt.Println(d)
}
```
