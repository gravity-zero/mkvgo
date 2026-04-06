# mkvgo

[![Go Reference](https://pkg.go.dev/badge/github.com/gravity-zero/mkvgo.svg)](https://pkg.go.dev/github.com/gravity-zero/mkvgo)
[![Go Report Card](https://goreportcard.com/badge/github.com/gravity-zero/mkvgo)](https://goreportcard.com/report/github.com/gravity-zero/mkvgo)
[![CI](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml/badge.svg)](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/gravity-zero/mkvgo/branch/master/graph/badge.svg)](https://codecov.io/gh/gravity-zero/mkvgo)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Pure Go MKV/WebM toolkit. Read, write, mux, split, edit MKV containers. Stdlib only, zero external dependencies.

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

Full CLI reference: [docs/cli.md](docs/cli.md)

### Examples

**Probe a file:**
```bash
mkvgo probe movie.mkv
mkvgo probe -json movie.mkv | jq '.tracks[]'
```

**Remove a track:**
```bash
# Remove track 3 (e.g. commentary audio)
mkvgo remove-track movie.mkv -o clean.mkv -t 3
```

**Edit metadata with JSON:**
```bash
# Via argument
mkvgo edit movie.mkv -o edited.mkv '{"title":"New Title"}'

# Via stdin (pipe from file or another tool)
cat meta.json | mkvgo edit movie.mkv -o edited.mkv -
```

**Split by time:**
```bash
# Split into two parts: 0-5min and 5min-end
mkvgo split movie.mkv -o parts/ -range 0-300000,300000-0
```

**Merge subtitles:**
```bash
mkvgo merge-subtitle movie.mkv -o output.mkv subs.srt -lang eng -name "English"
mkvgo merge-subtitle movie.mkv -o output.mkv subs.ass -format ass -lang jpn -name "Japanese"
```

**Extract subtitles:**
```bash
mkvgo extract-subtitle movie.mkv -t 3 -o subs.srt
mkvgo extract-subtitle movie.mkv -t 3 -o subs.ass -format ass
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
    c, err := matroska.Open(context.Background(), "movie.mkv")
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

**Edit metadata with custom FS (S3, HTTP, etc.):**
```go
s3fs := &matroska.FS{
    Open:   func(p string) (mkv.ReadSeekCloser, error) { /* S3 GetObject */ },
    Create: func(p string) (mkv.WriteSeekCloser, error) { /* S3 PutObject */ },
}

err := matroska.EditMetadata(ctx, "s3://bucket/movie.mkv", "s3://bucket/out.mkv",
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

matroska/          facade -- re-exports everything, backward compat

mkv/               core types, FS port, EBML IDs
  reader/          parse MKV → Container
  writer/          Container → MKV bytes
  ops/             high-level operations (mux, split, merge, edit...)
  subtitle/        SRT/ASS parsing

ebml/              low-level EBML encoding/decoding (no Matroska knowledge)
```

Import graph: `cmd/mkvgo` -> `matroska` -> `mkv/*` -> `ebml`

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
