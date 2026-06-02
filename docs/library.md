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

c, err := matroska.Open(ctx, "movie.mkv")
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
f, _ := os.Open("movie.mkv")
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
    SourcePath: "movie.mkv",
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
err := matroska.EditInPlace(ctx, "movie.mkv",
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
    SourcePath: "movie.mkv",
    OutputDir:  "./parts/",
    Ranges: []matroska.TimeRange{
        {StartMs: 0, EndMs: 300000},
        {StartMs: 300000, EndMs: 0},  // 0 = end of file
    },
})
// files = ["./parts/movie_001.mkv", "./parts/movie_002.mkv"]
```

**Split by chapters:**
```go
files, err := matroska.Split(ctx, matroska.SplitOptions{
    SourcePath: "movie.mkv",
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
err := matroska.ExtractSubtitle(ctx, "movie.mkv", trackID, "out.srt")

// As ASS
err := matroska.ExtractASS(ctx, "movie.mkv", trackID, "out.ass")
```

### Merge into MKV

```go
// SRT
err := matroska.MergeSubtitle(ctx, "movie.mkv", "subs.srt", "out.mkv", "eng", "English")

// ASS
err := matroska.MergeASS(ctx, "movie.mkv", "subs.ass", "out.mkv", "jpn", "Japanese")
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
issues, err := matroska.Validate(ctx, "movie.mkv")
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
