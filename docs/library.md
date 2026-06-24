# Library Usage Guide

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

`matroska` is the stable public API -- import it for most use cases. The `mkv`, `mkv/reader`, `mkv/writer`, `mkv/ops` and `mkv/subtitle` packages are lower-level and experimental: their APIs may change between minor versions. Import them directly when you need capabilities the facade does not expose (streaming, `NewWebMStreamWriter`).

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

`RemuxToWebM` validates the codecs, copies every block verbatim into time-bounded `webm` clusters, and rejects sources with non-WebM codecs. Elements outside the WebM subset (Chapters, Attachments, Tags) are dropped; list them beforehand with `matroska.WebMNonSubsetElements(container)`.

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

- **Video:** H.264, HEVC, AV1.
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

### MP4 → MKV

```go
err := mp4.RemuxFromMP4(ctx, "in.mp4", "out.mkv")
```

Reads `avc1`/`avc3`, `hvc1`/`hev1`, `av01`, `mp4a` (AAC, MP3 or DTS, by `esds` object type), `Opus`, `ac-3`, `ec-3`, `fLaC`, `tx3g` (→ SRT) and `wvtt` (→ WebVTT). Colour code points and chapters round-trip back to the Matroska `Colour` element and chapter atoms. Tracks with any other sample entry, and non-audio/video/subtitle tracks, are dropped.

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
| `FrameCount` | `nb_frames` | MP4 `stsz` count (0 for Matroska) |
| `Profile`, `Level` | `profile`, `level` | SPS / hvcC |
| `PixelFormat` | `pix_fmt` | chroma subsampling + bit depth (SPS / av1C / vpcC) |
| `FieldOrder` | `field_order` | Matroska `FlagInterlaced` / H.264 `frame_mbs_only_flag` |
| `VideoBitDepth` | bit depth | `colr`/Colour or codec bitstream |
| `ColorSpaceName()`, `ColorTransferName()`, `ColorPrimariesName()`, `ColorRangeName()`, `IsHDR()` | `color_space`/`color_transfer`/`color_primaries`/`color_range` | `colr` (nclx/nclc) / Matroska Colour / SPS VUI |
| `DolbyVision` | side data | MP4 `dvcC`/`dvvC` box / Matroska `BlockAdditionMapping` |
| `Channels` / `ChannelLayout()` | `channels` / `channel_layout` | codec config (HE-AACv2 PS counted) |
| `SampleRate`, `OutputSampleRate`, `EffectiveSampleRate()` | `sample_rate` | sample entry / Matroska; SBR output rate from the AAC ASC or `OutputSamplingFrequency` |

A few cases are genuinely not readable head-only (the data lives only in the media frames, so ffprobe decodes a frame): implicit in-band SBR / Parametric Stereo, and colour carried only in an in-band SPS. In those the probe reports the header value (e.g. the AAC core rate) rather than guessing. See the [CHANGELOG notes](../CHANGELOG.md).

**In-band colour fallback (opt-in).** Some streaming-style HEVC muxes keep the SPS in-band (a bare hvcC with no parameter sets) and write no container colour, so a head-only probe sees no colour at all. Passing the in-band option makes the probe — only for such a track — read its first sample, parse the SPS VUI, and apply an Alternative Transfer Characteristics SEI override (HLG's `bt2020-10` → `arib-std-b67` compatibility signal). It is off by default; tracks that already carry colour in the header read no frame.

```go
// MKV
c, _ := matroska.OpenMeta(ctx, "video.mkv", matroska.WithInBandColourFallback())
// MP4
c, _, _ := mp4.OpenMeta(ctx, "video.mp4", mp4.Options{InBandColour: true})
```

### Keyframe index (head-only)

To align `-c copy` HLS/DASH segments on source keyframes without a full packet scan, read `Container.Keyframes` — the metadata probe fills it in the same pass, no separate call or second open:

```go
// MKV/WebM: filled from the Cues index in the normal metadata pass.
c, err := matroska.OpenMeta(ctx, "video.mkv")
ks := c.Keyframes                                  // []int64 ms, ascending, de-duplicated

// MP4: opt-in, since building the sample table dominates the parse on a long movie.
c, _, err := mp4.OpenMeta(ctx, "video.mp4", mp4.Options{Keyframes: true})
ks = c.Keyframes
```

`Keyframes` holds the video track's keyframe presentation timestamps in milliseconds. MKV/WebM fills it from the `Cues` element reached via the `SeekHead` (one seek, no `Cluster` scan) in the normal metadata pass, and a full `matroska.Read` exposes it too. MP4 derives it from the `stss`/`stts`/`ctts` sample tables with the edit list (`elst`) applied as ffmpeg does — but only when `Options{Keyframes: true}` is set, because expanding the sample table is the dominant cost of parsing a long movie's `moov`; the default `mp4.OpenMeta` reads only the box headers and leaves `Keyframes` nil. It is nil when the source has no usable index, so the caller can fall back to a packet scan.

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
    Chapters: chapters,  // optional
    Tags:     tags,       // optional
})
```

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

Modifies the file directly without rewriting cluster data. Only safe for metadata changes that fit in the existing header space.

```go
err := matroska.EditInPlace(ctx, "video.mkv",
    func(c *matroska.Container) {
        c.Info.Title = "Quick Fix"
    },
)
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

**Split by chapters:**
```go
files, err := matroska.Split(ctx, matroska.SplitOptions{
    SourcePath: "video.mkv",
    OutputDir:  "./chapters/",
    ByChapters: true,
})
```

**Join sequential files:**
```go
err := matroska.Join(ctx, []string{"part1.mkv", "part2.mkv"}, "full.mkv")
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

The reader tolerates corrupted bodies: a zeroed or padded region between clusters (seen in some real-world rips) does not abort the read -- the parser resyncs to the next valid Cluster and returns the metadata gathered so far. A damaged EBML/Segment header still returns an error. Malformed input never panics.

```go
c, err := matroska.Open(ctx, path)
if err != nil {
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
