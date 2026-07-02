# mkvgo recipes

A task-oriented cookbook: pick a goal, copy the snippet. Each recipe shows the
**CLI** and the **Go library** side by side. For the exhaustive reference see
[cli.md](cli.md) and [library.md](library.md).

All library examples assume:

```go
import (
    "context"
    "github.com/gravity-zero/mkvgo/matroska" // MKV/WebM facade
    "github.com/gravity-zero/mkvgo/mp4"       // MP4 remux + probe
)

ctx := context.Background()
```

---

## Inspect a file

See everything inside a container — works on MKV/WebM **and** MP4.

**CLI** — `-json` works on any inspection command (`probe`, `tracks`, `info`,
`keyframes`…):

```bash
mkvgo probe video.mkv          # human-readable
mkvgo probe -json video.mkv    # structured (pipe to jq)
```

One track in the `-json` output:

```json
{
  "id": 1,
  "type": "video",
  "codec": "h264",
  "codec_long_name": "H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10",
  "width": 320,
  "height": 180,
  "profile": "High 4:4:4 Predictive",
  "level": 12,
  "pixel_format": "yuv444p",
  "field_order": "progressive",
  "colour_determined": true,
  "color_range": 1
}
```

**Library** — the same data, as typed structs:

```go
c, err := matroska.Open(ctx, "video.mkv")
if err != nil { /* ... */ }
fmt.Println(c.Info.Title, c.DurationMs, "ms")
for _, t := range c.Tracks {
    fmt.Printf("#%d %s %s lang=%s\n", t.ID, t.Type, t.Codec, t.ResolvedLanguage())
}
```

> **CLI and library are equivalent.** The `-json` output is exactly
> `json.Marshal` of the library's `*matroska.Container` — same field names and
> json tags. The CLI only adds three convenience fields the library exposes as
> *methods* instead of struct fields: `codec_long_name` (`Track.CodecLongName()`),
> `channel_layout` (`Track.ChannelLayout()`) and `avg_frame_rate`
> (`Track.AvgFrameRate()`). So `json.Marshal(c)` in your own code yields the
> identical structure, minus those three derived fields.

## Index a media library — fast, head-only

For library indexing you usually want stream metadata, not chapters/attachments
or a cluster walk. `OpenMeta` reads only the header and stops — orders of
magnitude faster than a full `Open` on a large file.

```go
c, err := matroska.OpenMeta(ctx, "video.mkv") // Tracks + Info only
// Per-track bitrate (the TAG:BPS ffprobe shows) stays head-only with the opt-in:
c, err = matroska.OpenMeta(ctx, "video.mkv", matroska.WithBitrate())
// MP4 counterpart (second value lists non-carried tracks like cover art;
// bitrate needs no opt-in there — MP4 carries it in btrt/esds):
mc, dropped, err := mp4.OpenMeta(ctx, "video.mp4")
```

## Keyframes for HLS/DASH segmentation

The cut points a `-c copy` segmenter must align on, without a full packet scan.

```bash
mkvgo keyframes video.mkv          # HH:MM:SS  <ms>  per line
mkvgo keyframes -json movie.mp4    # [0, 2000, 4000, ...]
```

```go
c, _ := matroska.OpenMeta(ctx, "video.mkv")
ks := c.Keyframes // []int64 ms, ascending — from the Cues index, head-only

// No Cues? Build the complete index from one sequential pass (no external probe):
c, _ = matroska.OpenMeta(ctx, "no-cues.mkv", matroska.WithKeyframeIndex())

// MP4 needs the opt-in (expanding the sample table is the dominant parse cost):
mc, _, _ := mp4.OpenMeta(ctx, "video.mp4", mp4.Options{Keyframes: true})
```

## Read colour / detect HDR

```go
c, _ := matroska.OpenMeta(ctx, "video.mkv")
t := c.Tracks[0]
fmt.Println(t.ColorSpaceName(), t.ColorTransferName(), t.IsHDR())
if t.HDR != nil {
    fmt.Println("HDR10 MaxCLL", t.HDR.MaxCLL, "MaxFALL", t.HDR.MaxFALL)
}
if t.DolbyVision != nil {
    fmt.Println("Dolby Vision profile", t.DolbyVision.Profile)
}
// ColourDetermined tells "confirmed SDR" (true, no values) from "unread" (false).
```

Some HDR streams keep their colour only in an in-band SPS. Opt in to recover it:

```go
c, _ := matroska.OpenMeta(ctx, "video.mkv", matroska.WithInBandColourFallback())
```

## Extract subtitles

```bash
mkvgo extract-subtitle video.mkv -t 3 -o subs.srt            # text → SRT
mkvgo extract-subtitle video.mkv -t 3 -o subs.ass -format ass # styled → ASS
mkvgo extract-subtitle video.mkv -t 3 -o subs.vtt -format vtt # → WebVTT
```

```go
err := matroska.ExtractSubtitle(ctx, "video.mkv", 3, "subs.srt")
```

## Convert MKV ↔ MP4 (no re-encode)

Remux copies the media verbatim — no quality loss, no transcode.

```bash
mkvgo to-mp4 --faststart video.mkv video.mp4   # moov first → progressive HTTP
mkvgo to-mp4 --skip-unsupported in.mkv out.mp4  # drop tracks MP4 can't carry
mkvgo from-mp4 video.mp4 video.mkv
```

```go
err := mp4.RemuxToMP4(ctx, "video.mkv", "video.mp4", mp4.Options{FastStart: true})
err = mp4.RemuxFromMP4(ctx, "video.mp4", "video.mkv")
```

Codecs carried: H.264/HEVC/AV1/VP9 video; AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio;
SRT/WebVTT → tx3g (or lossless `wvtt` with `--webvtt-native`). Chapters,
colour/HDR and B-frame ordering are preserved.

## Convert to WebM

```bash
mkvgo to-webm video.mkv video.webm   # VP8/VP9/AV1 + Vorbis/Opus only
```

```go
err := matroska.RemuxToWebM(ctx, "in.mkv", "out.webm")
```

## Package for HLS (no transcode)

Produce a fragmented-MP4 / CMAF HLS presentation — the "copy rung" that lets a
browser play an MKV without re-encoding. Segments are cut on keyframes and are
independently decodable.

```bash
mkvgo to-hls video.mkv -o stream/ -segment 6
# serve stream/ over HTTP; play stream/master.m3u8 (hls.js / Safari / ffmpeg)
```

```go
err := mp4.RemuxToHLS(ctx, "video.mkv", "stream/", mp4.Options{SegmentMs: 6000})
```

---

> The editing and assembly recipes below (add/remove track, merge, split, join,
> edit) operate on **Matroska**. To apply them to MP4 content, remux it to MKV
> first (`from-mp4`), operate, then remux back (`to-mp4`) — every step is lossless.

## Add or remove a track

```bash
mkvgo remove-track video.mkv -o clean.mkv -t 3        # drop track 3
mkvgo add-track video.mkv -o out.mkv audio.mkv:1      # add track 1 of audio.mkv
```

```go
err := matroska.RemoveTrack(ctx, "video.mkv", "clean.mkv", []uint64{3})
```

## Merge an external subtitle

```bash
mkvgo merge-subtitle video.mkv -o out.mkv subs.srt -lang eng -name "English"
```

```go
err := matroska.MergeSubtitle(ctx, "video.mkv", "subs.srt", "out.mkv", "eng", "English")
```

## Split by time or chapters

```bash
mkvgo split video.mkv -o parts/ -range 0-300000,300000-0  # 0–5 min, then 5 min–end (ms)
mkvgo split video.mkv -o parts/ -chapters                  # one part per chapter
```

```go
out, err := matroska.Split(ctx, matroska.SplitOptions{
    SourcePath: "video.mkv", OutputDir: "parts/",
    Ranges: []matroska.TimeRange{{StartMs: 0, EndMs: 300000}},
})
// ...or ByChapters: true for one part per chapter.
```

## Join files

Concatenate clips with the same track layout (each track is rebased on its own
end, so audio and video stay in sync).

```bash
mkvgo join part1.mkv part2.mkv part3.mkv -o full.mkv
```

```go
err := matroska.Join(ctx, []string{"part1.mkv", "part2.mkv"}, "full.mkv")
```

## Edit metadata — including instant in-place

A full edit rewrites the file (copying clusters). An *in-place* edit rewrites only
the header region, so it is instant regardless of file size.

```bash
mkvgo edit-title video.mkv -o out.mkv "New Title"
mkvgo edit-track video.mkv -o out.mkv -t 2 -lang fre -name "Français" -default
mkvgo edit-inplace video.mkv '{"title":"New Title"}'   # instant, no rewrite
```

```go
err := matroska.EditMetadata(ctx, "in.mkv", "out.mkv", func(c *matroska.Container) {
    c.Info.Title = "New Title"
    c.Tracks[1].Name = "Français"
})
err = matroska.EditInPlace(ctx, "video.mkv", func(c *matroska.Container) {
    c.Info.Title = "New Title"
})
```

## Read from a pipe / non-seekable stream

```bash
cat video.mkv | mkvgo probe -    # '-' reads stdin via the streaming reader
```

```go
import "github.com/gravity-zero/mkvgo/mkv/reader"

c, blocks, err := reader.ReadStream(ctx, os.Stdin) // metadata + a block reader
```

## Use a custom filesystem (S3, HTTP, …)

Every operation accepts an `FS` port, so the same code runs against object storage
or any backend.

```go
fs := &matroska.FS{
    Open:   func(p string) (mkv.ReadSeekCloser, error)  { /* S3 GetObject */ },
    Create: func(p string) (mkv.WriteSeekCloser, error) { /* S3 PutObject */ },
}
err := matroska.EditMetadata(ctx, "s3://bucket/in.mkv", "s3://bucket/out.mkv",
    func(c *matroska.Container) { c.Info.Title = "Updated" },
    matroska.Options{FS: fs},
)
```
