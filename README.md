# mkvgo

[![Go Reference](https://pkg.go.dev/badge/github.com/gravity-zero/mkvgo.svg)](https://pkg.go.dev/github.com/gravity-zero/mkvgo)
[![Go Report Card](https://goreportcard.com/badge/github.com/gravity-zero/mkvgo)](https://goreportcard.com/report/github.com/gravity-zero/mkvgo)
[![CI](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml/badge.svg)](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/gravity-zero/mkvgo/branch/master/graph/badge.svg)](https://codecov.io/gh/gravity-zero/mkvgo)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Pure Go MKV/WebM toolkit. Read, write, mux, split, edit MKV containers, and remux to/from MP4. Stdlib only, zero external dependencies.

CLI tool and importable Go library.

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
| **Inspection** | `info` | Show container info (title, duration, muxing app) |
| | `tracks` | List all tracks with codec, language, resolution |
| | `chapters` | List chapters with timestamps |
| | `attachments` | List attachments (fonts, images) |
| | `tags` | Show all tags |
| | `probe` | Full dump of all metadata |
| | `validate` | Check MKV structure for issues |
| | `compare` | Diff metadata of two MKV files |
| **Extraction** | `demux` | Extract tracks to raw streams |
| | `extract-attachment` | Extract an attachment to file |
| | `extract-subtitle` | Extract subtitle track as SRT or ASS |
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
# MKV/WebM → MP4 (H.264/HEVC/AV1 + AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS, SRT→tx3g,
# chapters, colour/HDR preserved)
mkvgo to-mp4 video.mkv video.mp4

# moov before mdat (progressive HTTP), and drop tracks MP4 can't carry (e.g. TrueHD)
mkvgo to-mp4 --faststart --skip-unsupported video.mkv video.mp4

# MP4 → MKV
mkvgo from-mp4 video.mp4 video.mkv
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
// H.264/HEVC/AV1 video; AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio; SRT→tx3g;
// chapters, colour/HDR and B-frame ordering preserved. Options{FastStart:true}
// writes moov first; Options{SkipUnsupported:true} drops unsupported tracks.
err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4")
err = mp4.RemuxFromMP4(ctx, "in.mp4", "out.mkv")
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

mp4/               MP4 (ISO-BMFF) remux to/from MKV (experimental, isolated from EBML core)
```

Import graph: `cmd/mkvgo` -> `matroska` -> `mkv/*` -> `ebml`; `mp4` -> `mkv/*`

## Build

```bash
make build                # build for current platform
make test                 # run tests with -race
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
