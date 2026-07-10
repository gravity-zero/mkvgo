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

## Package for streaming (HLS + DASH, no transcode)

Turn a file into a CMAF presentation — one demuxed segment set described by
**both** an HLS `master.m3u8` and a DASH `manifest.mpd`. Segments are cut on
keyframes and independently decodable. Works on MKV/WebM **and** MP4/MOV.

```bash
mkvgo to-hls video.mkv -o stream/ -segment 6
# serve stream/ over HTTP; play master.m3u8 (hls.js/Safari) or manifest.mpd (dash.js)
```

```go
err := mp4.RemuxToHLS(ctx, "video.mkv", "stream/", mp4.Options{SegmentMs: 6000})
```

Serve **on demand** instead (zero pre-generation) — each resource built when
requested, an HTTP handler in one call:

```go
plan, _ := mp4.PlanHLS(ctx, "video.mkv", mp4.Options{SegmentMs: 6000})
data, mime, _ := plan.Resource(ctx, "seg00042.m4s") // name = what the player requests
```

Encrypt (AES-128) and sign URLs:

```bash
mkvgo to-hls video.mkv -o stream/ --aes-key <32-hex> --aes-key-uri https://…/key
```

**Rotate the key** for forward secrecy (a captured key decrypts only its
period, not the whole video):

```bash
mkvgo to-hls video.mkv -o stream/ --aes-rotate-segments 10 \
  --aes-key <hexA>,<hexB> --aes-key-uri https://…/key/a,https://…/key/b
```

**Common Encryption** for the EME/DASH path (Widevine/PlayReady/FairPlay CDMs),
H.264/HEVC/AV1/VP9:

```bash
mkvgo to-hls video.mkv -o stream/ --cenc-scheme cenc \
  --cenc-key <32-hex> --cenc-kid <32-hex> --cenc-iv <16-hex> --cenc-key-uri https://…/key
```

→ The full streaming guide — ABR, single-file, trick-play, remote/S3 sources,
browser playback — is **[streaming.md](streaming.md)**.

## Forensic A/B watermarking (trace a leak)

Serve one stream whose per-segment bytes are drawn from one of two GOP-aligned
encodes by a per-viewer bit pattern, so a leaked copy carries a signature
identifying the session. No re-encode; the manifest is shared, only the
per-segment A/B choice differs.

```bash
# serve segment 7 from variant B for this viewer (or route by --pattern <hex>)
mkvgo watermark-segment a.mkv b.mkv 7 --variant B -o seg.m4s
```

```go
wm, _ := mp4.PlanWatermark(ctx, "a.mkv", "b.mkv", mp4.Options{SegmentMs: 6000})
seg, _ := wm.Segment(ctx, n, sessionBit(n)) // A or B for this viewer's code
```

The code assignment (which session gets which bits, collusion-resistant codes)
is the caller's policy. WASM: `openWatermark(a, b)`.

## Deduplicate uploads (content fingerprint)

Per-track digests of the compressed samples, identical across containers, so
the same encode fingerprints the same whether it arrives as MKV, WebM or MP4 -
an "you already uploaded this" check.

```bash
mkvgo fingerprint movie.mkv     # same digests as the equivalent movie.mp4
mkvgo fingerprint movie.mp4
```

## Thumbnails / scrubbing storyboard

Pull the keyframe nearest a time, decoder-ready, then make the image with ffmpeg
(mkvgo never decodes):

```bash
mkvgo extract-frame movie.mkv 00:12:30 -o frame.h264
ffmpeg -i frame.h264 -frames:v 1 thumb.jpg
```

```go
ks, _ := matroska.ExtractKeyframeSample(ctx, "movie.mkv", 750_000) // ms
// ks.Data is Annex-B (H.264/HEVC) or IVF (VP8/VP9/AV1); ks.Ext, ks.PtsMs
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

Ready-made ports: **`mkv.NewMemFS()`** (in-memory — the wasm build's foundation,
handy in tests) and **`httpfs.New()`** (HTTP Range — probe or package straight
from a URL, transferring only the bytes you read):

```go
import "github.com/gravity-zero/mkvgo/httpfs"

f := httpfs.New()
c, _ := matroska.OpenMetaWithFS(ctx, "https://nas/movie.mkv", f.Port())
fmt.Println(c.Tracks, f.BytesFetched()) // head-only: a few KB, whatever the size
```

The CLI accepts `http(s)://` URLs on the inspection commands and as a `to-hls`/
`hls-segment` source directly.
