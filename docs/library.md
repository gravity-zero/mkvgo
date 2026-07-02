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

The zero-storage counterpart of `RemuxToHLS`: `PlanHLS` performs a few bounded
reads (the metadata head with its Cues, the first and last clusters) and each
`Segment(n)` then seeks straight to its window through the Cues and reads only
that window — a server answers any HLS request in milliseconds with nothing
pre-generated. Combined with an `httpfs` source, only the ranges a viewer
actually watches are ever transferred from remote storage.

The fragments are built by the same code as `RemuxToHLS`, so every resource is
**byte-identical** to the full pass (regression-tested) — pre-generated and
on-demand serving mix transparently. Cover art and global tags ride in the
init segment (`WithAttachments`/`WithTags` reach them through the SeekHead,
still head-only). Text subtitle tracks are declared in the master playlist and
served as one whole-presentation WebVTT rendition each (`subN.m3u8` +
`subN.vtt`) — text blocks have no cue index, so a `.vtt` costs one sequential
pass, lazily; cache it if requested often. The remaining difference: the
master `BANDWIDTH` is estimated from cluster sizes. The source must carry a
Cues index. The plan is immutable and safe for concurrent `Segment` calls.

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
incompatible with `Encrypt`.

### ABR light (`mp4.RemuxToABR`)

```go
err := mp4.RemuxToABR(ctx, []string{"1080p.mkv", "720p.mkv"}, "stream/", mp4.Options{SegmentMs: 6000})
```

Packages pre-encoded quality variants (best first) into one multi-variant HLS
master — the packaging half of ABR, no transcoding. The first source is the
reference (audio + subtitles for every variant, from `v1/`); the others are
packaged `Options.VideoOnly`. Every variant gets its real
`BANDWIDTH`/`RESOLUTION`/`CODECS` in the top `master.m3u8`. HLS-only as a
combined presentation (per-variant `manifest.mpd` inside each directory);
sources should share GOP cadence for seamless switching.

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

`RewriteURL` rewrites every URI the playlists and the MPD reference. Resource
names stay canonical: the server strips its decoration (query token, prefix)
before calling `plan.Resource(ctx, name)`, and verifies the signature — the
hook makes every segment URL individually signed and expirable.

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

```go
import "github.com/gravity-zero/mkvgo/mkv/ops"

err := ops.Reindex(ctx, "input.mkv", "output.mkv")
```

With a progress callback:

```go
err := ops.Reindex(ctx, "input.mkv", "output.mkv", mkv.Options{
    Progress: func(processed, total int64) {
        fmt.Printf("\r%.1f%%", float64(processed)/float64(total)*100)
    },
})
```

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
