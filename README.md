# mkvgo

[![Go Reference](https://pkg.go.dev/badge/github.com/gravity-zero/mkvgo.svg)](https://pkg.go.dev/github.com/gravity-zero/mkvgo)
[![Go Report Card](https://goreportcard.com/badge/github.com/gravity-zero/mkvgo)](https://goreportcard.com/report/github.com/gravity-zero/mkvgo)
[![CI](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml/badge.svg)](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/gravity-zero/mkvgo/branch/master/graph/badge.svg)](https://codecov.io/gh/gravity-zero/mkvgo)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

In-process MKV/WebM container toolkit in pure Go. Probe, edit, remux to/from MP4 - no ffmpeg, no cgo, zero dependencies.

mkvgo is both a **command-line tool** and an **importable Go library** for
Matroska (`.mkv`/`.webm`) and MP4 (`.mp4`/`.mov`). Use it to read what's inside a
file (codecs, languages, colour/HDR, chapters, keyframes...), index a media
library fast, prepare files for streaming, convert between MKV and MP4, extract or
merge subtitles, or edit metadata - without shelling out to an external tool.

### Supported formats

| Format | Inspect / probe | Edit · mux · split · join | Convert to |
|---|:---:|:---:|---|
| **Matroska** `.mkv` | ✅ | ✅ | MP4, WebM |
| **WebM** `.webm` | ✅ | ✅ (WebM-subset codecs) | MP4, WebM |
| **MP4 / MOV** `.mp4` `.mov` | ✅ | - (remux only) | MKV |

WebM is read and written as Matroska. MP4/MOV is fully inspected (head-only, like
ffprobe) and remuxed to/from MKV - with no re-encoding in either direction.

Editing, muxing, splitting and joining are **Matroska operations**: to do them on
MP4 content, remux it to MKV first (`from-mp4`), operate, then remux back
(`to-mp4`) - every step is lossless (`-c copy`).

### Why mkvgo?

- **Zero dependencies.** Stdlib only. A single ~2 MB static binary that
  cross-compiles to Linux/macOS/Windows; or embed the library directly in your Go
  service - no subprocess, no ffmpeg to ship.
- **Fast metadata.** Probing is *head-only*: it reads the container headers, not
  the whole file, and never decodes a frame - so indexing a large library is
  orders of magnitude faster than a full demux.
- **ffprobe-equivalent fields.** Codecs, profile/level, pixel format, colour and
  HDR (HDR10 static metadata **and** Dolby Vision), aspect ratio, rotation,
  per-track bitrate, keyframes... mapped to the names ffprobe uses.
- **A full toolkit, not just a reader.** Write, mux, split, join, remux
  (MKV↔MP4, →WebM), convert subtitles, and edit metadata - including an *instant*
  in-place edit that rewrites only the header.
- **Built for real-world files.** Corruption-tolerant parsing (resyncs past
  damaged regions), bounded memory on hostile input, and continuously fuzzed.

### See it in action

```console
$ mkvgo probe video.mkv
File:        video.mkv
Title:       Regression Fixture
Duration:    00:00:06 (6023 ms)
MuxingApp:   Lavf60.16.100

Tracks (3):
  #1  video     h264        lang=und    name=""  320x180
        codec: profile="High 4:4:4 Predictive" level=12 pix_fmt=yuv444p field_order=progressive
        colour: unspecified (determined — SDR)
  #2  audio     aac         lang=fre    name="French"  44100Hz  1ch(mono)  [default]
  #3  audio     aac         lang=eng    name="English"  44100Hz  1ch(mono)

Keyframes: 6 (first 00:00:00, last 00:00:05)

Chapters (2):
  Chapter One  [00:00:00 - 00:00:03]
  Chapter Two  [00:00:03 - 00:00:06]
```

Add `-json` to any inspection command for machine-readable output.

New here? Start with the **[recipes](docs/recipes.md)** - common tasks, copy-paste
ready. For the full reference see **[docs/cli.md](docs/cli.md)** (CLI) and
**[docs/library.md](docs/library.md)** (Go library).

## Install

```bash
go install github.com/gravity-zero/mkvgo/cmd/mkvgo@latest
```

Or as a library:

```bash
go get github.com/gravity-zero/mkvgo
```

## CLI

```
mkvgo <command> [options]
```

Global flags: `-json` (structured output), `--version`

### Command Reference

| Category | Command | Description |
|---|---|---|
| **Inspection** | `info` | Show container info (title, duration, muxing app) - MKV or MP4 |
| | `tracks` | List all tracks with codec, language, resolution, Dolby Vision - MKV or MP4 |
| | `chapters` | List chapters with timestamps - MKV or MP4 |
| | `attachments` | List attachments (fonts, images) |
| | `tags` | Show all tags |
| | `probe` | Full dump of all metadata (ffprobe-equivalent stream fields: colour/HDR, Dolby Vision, pix_fmt, aspect ratio, rotation, bitrate, keyframes, dropped tracks…) - MKV or MP4 |
| | `keyframes` | List video keyframe timestamps (MKV Cues, or a complete structural scan when absent; MP4 sample table) |
| | `validate` | Check MKV structure for issues |
| | `compare` | Diff metadata of two MKV files |
| **Extraction** | `demux` | Extract tracks to raw streams |
| | `extract-attachment` | Extract an attachment to file |
| | `extract-subtitle` | Extract subtitle track as SRT, ASS or WebVTT (MKV or MP4) |
| | `to-vtt` | Convert an external `.srt`/`.ass`/`.vtt` sidecar to WebVTT |
| **Editing** | `edit` | Edit metadata from JSON (arg or stdin) |
| | `edit-title` | Change the container title |
| | `edit-track` | Edit track properties (lang, name, default, forced) |
| | `edit-inplace` | Edit metadata without rewriting clusters (instant) |
| | `remove-track` | Remove tracks from an MKV |
| | `add-track` | Add a track from another MKV |
| **Assembly** | `mux` | Combine tracks into a single MKV |
| | `merge` | Combine all tracks from multiple MKVs |
| | `merge-subtitle` | Inject an external SRT/ASS into an MKV |
| | `join` | Concatenate multiple MKVs sequentially |
| **Splitting** | `split` | Split MKV by time ranges or chapters |
| **Indexing** | `reindex` | Rebuild the seek index (Cues) of a file |
| **Remux** | `to-mp4` | Remux MKV/WebM to MP4 (`--faststart`, `--skip-unsupported`) |
| | `from-mp4` | Remux MP4 to MKV |
| | `to-webm` | Remux MKV/WebM to WebM (WebM-subset codecs only) |

Full CLI reference: [docs/cli.md](docs/cli.md)

### Examples

**Probe a file:**
```bash
mkvgo probe video.mkv
mkvgo probe -json video.mkv | jq '.tracks[]'
```

**Read metadata from stdin (pipe-friendly):**

The inspection commands (`info`, `tracks`, `chapters`, `attachments`, `tags`, `probe`) accept `-` as the input path and read from stdin via the streaming reader.

```bash
cat video.mkv | mkvgo info -
cat video.mkv | mkvgo tracks -json -
```

**Remove a track:**
```bash
# Remove track 3 (e.g. commentary audio)
mkvgo remove-track video.mkv -o clean.mkv -t 3
```

**Edit metadata with JSON:**
```bash
# Via argument
mkvgo edit video.mkv -o edited.mkv '{"title":"New Title"}'

# Via stdin (pipe from file or another tool)
cat meta.json | mkvgo edit video.mkv -o edited.mkv -
```

**Split by time:**
```bash
# Split into two parts: 0-5min and 5min-end
mkvgo split video.mkv -o parts/ -range 0-300000,300000-0
```

**Merge subtitles:**
```bash
mkvgo merge-subtitle video.mkv -o output.mkv subs.srt -lang eng -name "English"
mkvgo merge-subtitle video.mkv -o output.mkv subs.ass -format ass -lang jpn -name "Japanese"
```

**Extract subtitles:**
```bash
mkvgo extract-subtitle video.mkv -t 3 -o subs.srt
mkvgo extract-subtitle video.mkv -t 3 -o subs.ass -format ass
```

**Remux to/from MP4 (no transcode):**
```bash
# MKV/WebM → MP4 (H.264/HEVC/AV1 + AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS,
# SRT/WebVTT→tx3g, chapters, colour/HDR preserved)
mkvgo to-mp4 video.mkv video.mp4

# Subtitles: ASS/SSA → tx3g (lossy), or WebVTT → lossless native wvtt (Apple/CMAF)
mkvgo to-mp4 --flatten-subs anime.mkv anime.mp4
mkvgo to-mp4 --webvtt-native web.mkv web.mp4

# moov before mdat (progressive HTTP), and drop tracks MP4 can't carry (e.g. TrueHD)
mkvgo to-mp4 --faststart --skip-unsupported video.mkv video.mp4

# MP4 → MKV
mkvgo from-mp4 video.mp4 video.mkv

# MKV/WebM → WebM (VP8/VP9/AV1 + Vorbis/Opus only; other codecs rejected)
mkvgo to-webm video.mkv video.webm
```

## Library Usage

Full library guide: [docs/library.md](docs/library.md)

Import the facade package for convenience, or import sub-packages directly.

**Read metadata:**
```go
package main

import (
    "context"
    "fmt"
    "github.com/gravity-zero/mkvgo/matroska"
)

func main() {
    c, err := matroska.Open(context.Background(), "video.mkv")
    if err != nil { panic(err) }

    fmt.Println(c.Info.Title, c.DurationMs, "ms")
    for _, t := range c.Tracks {
        fmt.Printf("  #%d %s %s (%s)\n", t.ID, t.Type, t.Codec, t.Language)
    }
}
```

For library indexing, prefer `matroska.OpenMeta` (or `mp4.OpenMeta` for MP4): it returns the same Info + Tracks but stops as soon as both are parsed - never walking Clusters/Cues - so it is orders of magnitude faster than the full `Open`. Use `Open` only when you also need Chapters/Attachments/Tags/Cues. Opt-in read options extend the head-only probe: `WithBitrate()` (per-track BPS bitrate from the Tags element), `WithKeyframeIndex()` (complete keyframe index for a Cues-less file), `WithInBandColourFallback()` (colour from the first sample's SPS).

**Mux tracks from multiple sources:**
```go
err := matroska.Mux(ctx, matroska.MuxOptions{
    OutputPath: "output.mkv",
    Tracks: []matroska.TrackInput{
        {SourcePath: "video.mkv", TrackID: 1},
        {SourcePath: "audio.mkv", TrackID: 1, Language: "eng", Name: "Stereo"},
    },
})
```

**Rebuild the seek index of a file:**
```go
err := ops.Reindex(ctx, "input.mkv", "output.mkv")
```

**Read or write from a non-seekable stream (pipe, network):**

`reader.ReadStream` parses metadata and returns a `*BlockReader` from an `io.Reader` without ever calling Seek. `writer.NewStreamWriter` writes a live MKV stream to an `io.Writer` using unknown-size Segment and Clusters. See [docs/library.md](docs/library.md) for details.

**Remux a file to WebM:**
```go
// Validates the codecs (VP8/VP9/AV1, Vorbis/Opus, WebVTT), copies the media
// verbatim into a webm-DocType container, rejects non-WebM codecs.
err := matroska.RemuxToWebM(ctx, "in.mkv", "out.webm")
```

**Remux to/from MP4 (no transcode):**
```go
// H.264/HEVC/AV1 video; AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio; SRT/WebVTT→tx3g
// (Options{NativeWebVTT:true} keeps WebVTT lossless as wvtt; Options{FlattenStyledSubs:true}
// carries ASS/SSA as tx3g); chapters, colour/HDR and B-frame ordering preserved.
// Options{FastStart:true} writes moov first; Options{SkipUnsupported:true} drops tracks.
err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4")
err = mp4.RemuxFromMP4(ctx, "in.mp4", "out.mkv")
```

**Probe an MP4's metadata without remuxing (fast path for indexing):**
```go
// Reads only the moov box - the ffprobe-equivalent stream fields (codecs, profile/
// level, pixel format, colour/HDR, Dolby Vision, aspect ratio, rotation, frame rate,
// bitrate, channels/layout, per-track duration, file tags…) - never the sample data.
// Counterpart of matroska.OpenMeta. See docs/library.md for the full field table.
// The second value lists non-carried tracks (cover art, hint/timecode).
c, dropped, err := mp4.OpenMeta(ctx, "video.mp4")
fmt.Println(c.DurationMs, len(c.Tracks), "tracks,", len(dropped), "dropped")
```

**Edit metadata with custom FS (S3, HTTP, etc.):**
```go
s3fs := &matroska.FS{
    Open:   func(p string) (mkv.ReadSeekCloser, error) { /* S3 GetObject */ },
    Create: func(p string) (mkv.WriteSeekCloser, error) { /* S3 PutObject */ },
}

err := matroska.EditMetadata(ctx, "s3://bucket/video.mkv", "s3://bucket/out.mkv",
    func(c *matroska.Container) {
        c.Info.Title = "Updated"
    },
    matroska.Options{FS: s3fs},
)
```

## Limitations

- **No transcoding.** Every operation copies compressed samples verbatim; a
  codec the target container cannot carry is rejected or dropped (reported),
  never converted.
- **MP4 output codec set**: H.264/HEVC/AV1 video, AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS
  audio. VP8/VP9/Vorbis, TrueHD and bitmap subtitles (PGS/VOBSUB) cannot go to
  MP4. WebM output accepts only the WebM subset (VP8/VP9/AV1, Vorbis/Opus, WebVTT).
- **Elements dropped by remuxes**: `to-webm` drops chapters, attachments and
  tags; `to-mp4` drops non-image attachments (fonts), track-targeted tags and
  unmapped global tags — cover art is carried as `covr` (see
  [docs/cli.md](docs/cli.md)). The live `StreamWriter` output carries no
  SeekHead/Cues (impossible without seeking back).
- **Timing resolution**: operations that rebuild clusters (mux, merge, split,
  join, edit, remux) work on millisecond-quantised timecodes — exact for the
  default Matroska `TimecodeScale` (1 ms); finer scales are quantised.
  `reindex` copies clusters verbatim and is exempt. MP4→MKV→MP4 audio
  round-trips are sample-exact except Opus/MP3 tail padding.
- **Parser bounds** (anti-DoS, not configurable): 512 MB per EBML element,
  64 MB per block, 1 GiB of cumulative metadata, 256 MB per cluster on
  reindex. A legitimate file beyond these limits is rejected with an explicit
  error.

## Versioning

Pre-1.0 (SemVer, [Keep a Changelog](CHANGELOG.md)). The `matroska` facade is
the stable public API: held additive and backward-compatible across 0.x
releases by policy. The `mkv/*`, `mp4` and `ebml` sub-packages are
experimental and may change between minor versions.

## Documentation

- **[docs/recipes.md](docs/recipes.md)** - task-oriented cookbook: probe, index a
  library, keyframes for HLS, HDR, subtitles, remux, split/join, edit… copy-paste
  ready (CLI **and** Go side by side). Start here.
- **[docs/cli.md](docs/cli.md)** - full CLI reference: every command, flag and
  output field.
- **[docs/library.md](docs/library.md)** - full library guide: the API, the
  head-only probe field table (mapped to ffprobe), streaming, custom FS, and the
  capabilities a head-only read cannot reach.
- **[pkg.go.dev](https://pkg.go.dev/github.com/gravity-zero/mkvgo)** - generated
  Go API reference.

## Architecture

```
cmd/mkvgo/         CLI binary
  commands/        one file per command group

matroska/          facade -- stable public API, re-exports everything

mkv/               core types, FS port, EBML IDs (experimental, may change)
  reader/          parse MKV → Container
  writer/          Container → MKV bytes
  ops/             high-level operations (mux, split, merge, edit, remux-webm...)
  subtitle/        SRT/ASS parsing

ebml/              low-level EBML encoding/decoding (no Matroska knowledge)

mp4/               MP4 (ISO-BMFF) remux to/from MKV (isolated from EBML core)
```

Import graph: `cmd/mkvgo` -> `matroska` -> `mkv/*` -> `ebml`; `mp4` -> `mkv/*`

## Build

```bash
make build                # build for current platform
make test                 # run tests with -race
make vet                  # go vet
make fuzz                 # run the parser fuzzers locally (30s each)
make release              # cross-compile all platforms
```

`make release` produces stripped binaries (~2.3 MB) in `dist/`:

| Platform | Output |
|---|---|
| Linux amd64 | `dist/mkvgo-linux-amd64` |
| Linux arm64 | `dist/mkvgo-linux-arm64` |
| Windows amd64 | `dist/mkvgo-windows-amd64.exe` |
| macOS amd64 | `dist/mkvgo-darwin-amd64` |
| macOS arm64 | `dist/mkvgo-darwin-arm64` |

Build for a specific platform manually:

```bash
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o mkvgo ./cmd/mkvgo/
```

Version is injected at build time via `-ldflags`:

```bash
go build -ldflags="-s -w -X main.version=1.0.0" -o mkvgo ./cmd/mkvgo/
```

## License

MIT
