# mkvgo

[![Go Reference](https://pkg.go.dev/badge/github.com/gravity-zero/mkvgo.svg)](https://pkg.go.dev/github.com/gravity-zero/mkvgo)
[![CI](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml/badge.svg)](https://github.com/gravity-zero/mkvgo/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/gravity-zero/mkvgo/branch/master/graph/badge.svg)](https://codecov.io/gh/gravity-zero/mkvgo)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Pure-Go media toolkit and streaming packager.** Probe, remux and edit
Matroska/WebM/MP4, then package them for HLS **and** DASH - all in-process, with
no external tools, no cgo and zero dependencies.

One static binary (about 11 MB, or an imported Go library, or a WebAssembly
module of about 9 MB that gzips to about 2.4 MB) that inspects media, converts between
containers without re-encoding, and turns a file into a ready-to-stream CMAF
presentation. It never transcodes: every operation copies the compressed
samples verbatim.

---

## What it does

mkvgo is three things in one tool. Pick the pillar you need:

### 1 · Inspect

Read what's inside a file - **head-only**: the metadata comes from the header,
without decoding a frame or reading the whole file, so the work is proportional
to the metadata rather than the file size and indexing a large library stays
fast even on multi-gigabyte sources. The exception is an MKV with neither a
SeekHead nor Cues: reaching what lies behind its clusters means reading every
cluster's header (never its payload).

- Codecs, profile/level, pixel format, aspect ratio, rotation, frame rate,
  per-track bitrate, channel layout, keyframes.
- Colour and **HDR** - HDR10 static metadata **and** Dolby Vision.
- Chapters, attachments, tags, languages - mapped to the conventional prober
  field names, with `-json` output identical to the library's structs.

```bash
mkvgo probe video.mkv          # or video.mp4 - same output
mkvgo probe -json video.mkv | jq '.tracks[]'
```

### 2 · Convert & edit

Losslessly move content between containers and rework its metadata.

- **Remux** MKV/WebM ↔ MP4/MOV, and MKV → WebM - `-c copy`, no quality loss.
- **Mux / split / join / merge** tracks; **add / remove** tracks and attachments.
- **Subtitles** - extract (SRT/ASS/WebVTT), merge an external sidecar, convert.
- **Edit metadata** - title, track flags, chapters - including an *instant*
  in-place edit that rewrites only the header, whatever the file size.
- **Self-verifying files** - store per-track content hashes, detect bit rot.

```bash
mkvgo to-mp4 --faststart video.mkv video.mp4    # progressive MP4, no re-encode
mkvgo edit-inplace video.mkv '{"title":"…"}'    # instant, no rewrite
```

### 3 · Stream (HLS + DASH)

Turn a file into a **CMAF** presentation - the packaging half of adaptive
streaming, no transcoder involved. **One segment set, two manifests.**

- **HLS** (`master.m3u8`) **and DASH** (`manifest.mpd`) over the same demuxed
  fragmented-MP4 segments; native **multi-audio** (VF/VO) and subtitle
  selection.
- **On-demand**: build any segment/playlist when requested - zero
  pre-generation, zero storage - or pre-generate everything. Both modes emit
  byte-identical output.
- **ABR** from pre-encoded qualities, **AES-128** encryption (with **key
  rotation**) + signed URLs, **CENC** for the DRM/EME path (AV1/VP9 included),
  **A/B forensic watermarking** (the single-source flavor is experimental session marking, robust neither to collusion nor to re-encoding), **single-file** byte-range serving, **I-frame**
  trick-play playlists, **thumbnails** for scrubbing, and **audio-only**
  (podcasts/music).
- **Serving-side repairs** - an unindexed source streams anyway (the seek
  index is synthesized in memory from one structure-only walk), and a constant
  A/V desync is cancelled per track in the init's edit list: read-only
  sources, nothing rewritten.
- Sources: MKV/WebM **or** MP4/MOV, local path **or** `http(s)://` URL (S3).

```bash
mkvgo to-hls video.mkv -o stream/    # → master.m3u8 + manifest.mpd + segments
# serve stream/ over HTTP; play in hls.js / dash.js / Safari
```

→ Full guide: **[docs/streaming.md](docs/streaming.md)**

---

## Runs anywhere

The same engine, four ways to reach it:

| Runtime | What | Where |
|---|---|---|
| **CLI** | one static binary, no deps | Linux / macOS / Windows |
| **Go library** | import the `matroska` / `mp4` packages | your service, in-process |
| **WebAssembly** | probe/remux/package in the browser (bounded memory, any file size) | client-side, no server - [docs/wasm.md](docs/wasm.md) |
| **Remote (`httpfs`)** | read over HTTP Range - probe a library on S3/HTTP for a few KB/file | any object storage |

Cross-cutting guarantees: **deterministic output** (same input → byte-identical
bytes, across runs and machines - safe for content-addressed storage), a
pluggable **FS port** (S3/HTTP/in-memory), corruption-tolerant parsing, bounded
memory on hostile input, and continuous fuzzing.

---

## Supported formats

| Format | Inspect / probe | Edit · mux · split · join | Remux to | Package (HLS/DASH) |
|---|:---:|:---:|---|:---:|
| **Matroska** `.mkv` | ✅ | ✅ | MP4, WebM | ✅ |
| **WebM** `.webm` | ✅ | ✅ (WebM-subset codecs) | MP4, WebM | ✅ |
| **MP4 / MOV** `.mp4` `.mov` | ✅ | ✅ (no attachments) | MKV, MP4 (fast start) | ✅ |

WebM is read and written as Matroska. MP4/MOV is edited natively: metadata in
place or into a new file, `remove-track`, `split`, `join`, `mux`, `merge`,
`add-track`, `merge-subtitle` (SRT as timed text) and `retime` copy the samples
and sample descriptions byte for byte and write only new tables. What MP4 has
no room for - attachments, styled ASS subtitles - goes through `from-mp4` →
operate → `to-mp4` (every step lossless). Fragmented MP4 is inspected and
packaged directly; to edit one, remux it to a progressive MP4 first.

**Codecs carried** (never converted): H.264 · HEVC · AV1 · VP9 video; AAC ·
Opus · AC-3 · E-AC-3 · FLAC · MP3 · DTS audio; SRT · WebVTT · ASS/SSA subtitles.

---

## See it in action

```console
$ mkvgo probe video.mkv
File:        video.mkv
Title:       Regression Fixture
Duration:    00:00:06 (6023 ms)
MuxingApp:   Lavf60.16.100

Tracks (3):
  #1  video     h264        lang=und    name=""  320x180
        codec: profile="High 4:4:4 Predictive" level=12 pix_fmt=yuv444p scan=progressive field_order=progressive
        colour: unspecified (determined - SDR)
  #2  audio     aac         lang=fre    name="French"  44100Hz  1ch(mono)  [default]
  #3  audio     aac         lang=eng    name="English"  44100Hz  1ch(mono)

Keyframes: 6 (first 00:00:00, last 00:00:05)

Chapters (2):
  Chapter One  [00:00:00 - 00:00:03]
  Chapter Two  [00:00:03 - 00:00:06]
```

Add `-json` to any inspection command for machine-readable output.

New here? Start with the **[recipes](docs/recipes.md)** - common tasks,
copy-paste ready, CLI **and** Go side by side.

## Install

Requires **Go 1.24 or newer**. Zero third-party dependencies: the module graph
is empty, so `go get` pulls nothing but mkvgo itself.
On 32-bit x86 (`GOARCH=386`), take Go 1.26.5 or newer within 1.26: earlier
1.26 releases can corrupt the heap on 386, which mkvgo's tests hit on 1.26.2 as "found bad pointer in Go heap" ([golang/go#80004](https://github.com/golang/go/issues/80004)).

```bash
go install github.com/gravity-zero/mkvgo/cmd/mkvgo@latest   # CLI
go get github.com/gravity-zero/mkvgo                        # library
```

## Command reference

```
mkvgo <command> [options]      # global: -json, -f/--force, --version
```

| Category | Command | Description |
|---|---|---|
| **Inspect** | `info` · `tracks` · `chapters` · `attachments` · `tags` | Show container / track / chapter / attachment / tag info - MKV or MP4 |
| | `probe` | Full metadata dump: standard-prober stream fields (colour/HDR, Dolby Vision, pix_fmt, aspect, rotation, bitrate, keyframes…) |
| | `keyframes` | Video keyframe timestamps (Cues / sample table, or a structural scan) |
| | `validate` | Structural **and** streaming-readiness checks (Cues, cue keying, durations…) |
| | `cue-health` | Head-only seek-index triage - spots files that look indexed but seek wrong, in milliseconds |
| | `diagnose` | One-call triage with remedies (MKV/WebM **or** MP4): index health, per-track audio delay, truncation verdict - routes each file to the right repair |
| | `hash` / `verify` | Store per-track content hashes / detect bit rot (self-verifying files) |
| | `compare` | Diff metadata, or block content with `-blocks` - several files on the right compare against their concatenation |
| | `analyze` | Per-track frame/keyframe counts, bitrate, GOP, duration, CFR/VFR - walks every block header, no decode |
| | `fingerprint` | Container-independent content identity: presentation hash + per-track payload SHA-256 |
| | `track-ends` | Where each track's content really ends: picture end and audio shortfall |
| | `playability` · `ladder` | Direct-play verdict per client target / recommended ABR rungs capped at the source |
| | `ingest` | One-call serving plan for a file: direct-play, remux to HLS, or transcode |
| **Extract** | `demux` | Extract tracks to raw streams |
| | `extract-subtitle` · `to-vtt` | Subtitle track → SRT/ASS/WebVTT, or PGS bitmaps → PNGs + manifest; external sidecar → WebVTT |
| | `extract-attachment` · `add-attachment` · `remove-attachment` | Manage attachments (MIME sniffed) |
| | `extract-frame` | Keyframe nearest a time, decoder-ready - thumbnail/storyboard pipelines |
| | `extract-frames` | Keyframe-only Matroska stream (every N seconds or at given times) |
| | `subtitle-index` | Record where every subtitle block sits, so later extractions seek instead of walking the file |
| **Edit** | `edit` · `edit-title` · `edit-track` | Edit metadata (from JSON, or targeted flags) - MKV/WebM and MP4/MOV |
| | `edit-inplace` | Edit metadata without rewriting the media - instant (MKV: header patch; MP4: crash-ordered moov swap) |
| | `set-chapters` · `extract-chapters` | Import/export chapters as OGM text (the standard chapter interchange format) |
| | `remove-track` · `add-track` | Remove / add a track |
| **Assemble** | `mux` · `merge` · `join` | Combine tracks / files |
| | `merge-subtitle` | Inject an external SRT/ASS |
| | `split` | Split by time ranges, chapters or fixed duration (`-every`) |
| **Index & repair** | `reindex` · `reindex-inplace` | Rebuild the seek index (Cues) - verified copy, or in-place patch (crash-safe journal, file-only permission) |
| | `salvage` | Surgical recovery of a damaged file - lying sizes fixed lossless, valid blocks around a gap kept; `--dry-run` maps the damage first |
| | `rollback` | Undo any repair from its inverse delta (`--rollback-delta`, typically <0.1% of the file) - no full backup copy needed |
| | `retime` | Cancel a constant A/V desync in place - MKV: 2 bytes per block under a crash-safe journal; MP4: the moov edit list, a few bytes whatever the size |
| **Convert** | `to-mp4` · `from-mp4` · `to-webm` | Remux between containers (no transcode); `to-mp4` on an MP4/MOV rewrites it fast start, sample for sample |
| **Stream** | `to-hls` | Package as CMAF - HLS + DASH over one demuxed segment set (AES-128 + key rotation, CENC AV1/VP9, single-file, I-frames, audio-only) |
| | `hls-segment` | Serve one HLS/DASH resource on demand - zero pre-generation (local file **or** URL) |
| | `to-abr` | Multi-variant HLS master from pre-encoded qualities (ABR packaging) |
| | `abr-segment` | Serve one ABR resource on demand, byte-identical to `to-abr` |
| | `concat-hls` · `concat-segment` | Several sources as one continuous HLS session - pre-generated, or one resource on demand |
| | `serve` · `serve-growing` | Long-running HTTP server for one file's on-demand HLS (or the raw file for direct play) / play while downloading |
| | `cmaf-mpd` · `cmaf-hls` | DASH manifest / HLS playlists over CMAF rungs an external encoder already produced - nothing re-packaged, rungs checked segment-aligned |
| | `watermark-segment` | Serve one segment of an A/B forensic-watermarked stream (per-viewer bit routing, no re-encode) |
| | `forensic-segment` | Single-source A/B watermark - variant B derived by dropping one disposable H.264 frame per segment, timing-compensated. Experimental session marking, robust neither to collusion nor to re-encoding |

Full CLI reference: **[docs/cli.md](docs/cli.md)**

## Library

```go
import "github.com/gravity-zero/mkvgo/matroska" // MKV/WebM facade
import "github.com/gravity-zero/mkvgo/mp4"       // MP4 remux/probe + streaming
```

**Probe (head-only, fast for indexing):**
```go
c, err := matroska.OpenMeta(ctx, "video.mkv")     // Tracks + Info, stops early
mc, dropped, err := mp4.OpenMeta(ctx, "video.mp4") // MP4 counterpart
```

**Remux (no transcode):**
```go
err := mp4.RemuxToMP4(ctx, "in.mkv", "out.mp4", mp4.Options{FastStart: true})
err = mp4.RemuxFromMP4(ctx, "in.mp4", "out.mkv")
err = matroska.RemuxToWebM(ctx, "in.mkv", "out.webm")
```

**Package for streaming:**
```go
// Pre-generate everything (HLS + DASH):
err := mp4.RemuxToHLS(ctx, "in.mkv", "stream/", mp4.Options{SegmentMs: 6000})

// …or serve on demand - one call per request, nothing stored:
plan, _ := mp4.PlanHLS(ctx, "in.mkv", mp4.Options{SegmentMs: 6000})
data, contentType, _ := plan.Resource(ctx, "seg00042.m4s") // any player-facing name
```

**Custom FS (S3, HTTP, in-memory):**
```go
err := matroska.EditMetadata(ctx, "s3://bucket/in.mkv", "s3://bucket/out.mkv",
    func(c *matroska.Container) { c.Info.Title = "Updated" },
    matroska.Options{FS: myFS})
```

Full library guide: **[docs/library.md](docs/library.md)**.

## Documentation

| Doc | For |
|---|---|
| **[recipes.md](docs/recipes.md)** | Task-first cookbook - pick a goal, copy the snippet (CLI + Go). **Start here.** |
| **[streaming.md](docs/streaming.md)** | The HLS/DASH/CMAF packager end to end: modes, sources, ABR, security, trick-play. |
| **[cli.md](docs/cli.md)** | Every command, flag and output field. |
| **[library.md](docs/library.md)** | The Go API, the head-only probe field table (conventional prober fields), streaming, FS port, determinism. |
| **[wasm.md](docs/wasm.md)** | The browser build: probe/remux/package client-side, TypeScript wrapper, React hooks, MSE demo. |
| **[pkg.go.dev](https://pkg.go.dev/github.com/gravity-zero/mkvgo)** | Generated Go API reference. |

**Runnable examples:**

| Example | What |
|---|---|
| [`examples/hls-server/`](examples/hls-server/) | On-demand HLS + DASH server (~90 lines) - `go run ./examples/hls-server -src movie.mkv` |
| [`web/example/`](web/example/) | Browser WASM demo - drag a file in, probe it, play it via MSE (no server) |
| [`web/mkvgo.ts`](web/mkvgo.ts) · [`web/react.ts`](web/react.ts) · [`web/vue.ts`](web/vue.ts) | Typed wrapper + React hooks + Vue composables (copy-paste) |
| [`scripts/wasm_smoke.mjs`](scripts/wasm_smoke.mjs) | Node end-to-end usage of the wasm build |

## Limitations

- **No transcoding.** Every operation copies compressed samples verbatim; a
  codec the target cannot carry is rejected or dropped (and reported), never
  converted.
- **Output codec sets.** MP4: H.264/HEVC/AV1/VP9 + AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS
  (VP8/Vorbis, TrueHD and bitmap subtitles PGS/VOBSUB cannot go to MP4). WebM:
  the WebM subset only (VP8/VP9/AV1, Vorbis/Opus, WebVTT).
- **Encryption, packaging only.** HLS AES-128 (whole-segment, with mid-stream
  **key rotation**) and Common Encryption (**CENC** `cenc`/`cbcs`, for the
  EME/DASH path, H.264/HEVC/AV1/VP9) are supported: mkvgo produces the
  ciphertext and boxes, you run the license server. Under `cenc`, a key and
  base IV pair must be unique per title: two titles sharing both share their
  keystream. The library takes the IV you give it; the CLI draws a random one
  when `--cenc-iv` is left out. LL-HLS (live ingest) is out of scope - see
  [docs/library.md](docs/library.md).
- **Timing resolution.** Cluster-rebuilding operations (mux/merge/split/join/
  edit/remux) use millisecond-quantised timecodes - exact for the default
  Matroska scale (1 ms). MP4→MKV→MP4 audio round-trips are sample-exact except
  Opus/MP3 tail padding.
- **Parser bounds** (anti-DoS, not configurable): 512 MB per EBML element,
  64 MB per block, 1 GiB cumulative metadata, 256 MB per cluster on reindex.

## Versioning

Pre-1.0 (SemVer, [Keep a Changelog](CHANGELOG.md)). Until 1.0, any package may
change between minor versions, and a release that breaks an exported API or
changes the bytes mkvgo writes names it in the CHANGELOG's Breaking section.
The `matroska` facade and the `mp4` package are the shortest path for most
uses; the `mkv/*` and `ebml` sub-packages hold the building blocks under them.

## Architecture

```
cmd/mkvgo/         CLI binary (one file per command group)
cmd/mkvgo-wasm/    WebAssembly entry point (global MkvGo object)

matroska/          facade over mkv/*, re-exports everything
mp4/               MP4 remux + the HLS/DASH/CMAF streaming packager
httpfs/            FS port over HTTP Range (remote/S3 sources)

mkv/               core types, FS port, MemFS, EBML IDs
  reader/ writer/  parse / emit MKV
  ops/             high-level operations (mux, split, merge, edit, thumbnails…)
  subtitle/        SRT/ASS parsing
ebml/              low-level EBML codec (no Matroska knowledge)

web/               TypeScript wrapper (mkvgo.ts) + React hooks + runnable demo
```

Import graph: `cmd/mkvgo` → `matroska` → `mkv/*` → `ebml`; `mp4` → `mkv/*`;
`httpfs` → `mkv`.

## Build

```bash
make build          # static binary for the current platform (CGO_ENABLED=0)
make test           # tests with -race
make wasm           # dist/wasm/mkvgo.wasm + wasm_exec.js
make wasm-smoke     # build + Node end-to-end smoke test
make e2e            # end-to-end verification against a real external decoder
                    #   (needs the tool on PATH, or MKVGO_E2E=docker:<container>; see scripts/e2e.sh)
make fuzz           # run the parser fuzzers locally
make release        # cross-compile all platforms → dist/
```

`make release` produces stripped ~11 MB binaries for Linux (amd64/arm64),
Windows (amd64) and macOS (amd64/arm64). Version is injected via
`-ldflags="-X main.version=…"`.

## License

MIT
