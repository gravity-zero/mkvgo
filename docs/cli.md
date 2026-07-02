# CLI Reference

The complete command/flag/output reference. New here? The
**[recipes](recipes.md)** are a gentler, task-first starting point.

```
mkvgo <command> [options]
```

Global flags:
- `-json` -- structured JSON output (info, tracks, chapters, attachments, tags, probe, keyframes, validate, compare; accepted but ignored by writing commands)
- `-f`, `--force` -- overwrite an existing output file. Without it, every command that writes a new file refuses to clobber an existing one (`out.mkv already exists`). `edit-inplace` is the exception: it modifies its input file by design.
- `--version` -- print version and exit
- `-h`, `--help` -- show help for a command

Exit codes: `0` success, `1` any error (bad usage, unreadable input, failed
operation). `validate` also exits `1` when error-severity issues are found
(`-strict` makes warnings fail too) and `compare` when the files differ, so
they can gate scripts (`mkvgo validate f.mkv && ...`).

---

## Inspection

### info

Show container info (title, duration, muxing/writing app).

```
mkvgo info [-json] <file.mkv|->
```

Pass `-` as the path to read from stdin (uses the streaming reader; Cues are not available).

```bash
mkvgo info video.mkv
mkvgo info -json video.mkv
cat video.mkv | mkvgo info -
```

### tracks

List all tracks with codec, language, resolution/channels.

```
mkvgo tracks [-json] <file.mkv|->
```

Pass `-` as the path to read from stdin.

```bash
mkvgo tracks video.mkv
cat video.mkv | mkvgo tracks -
```

### chapters

List chapters with start/end timestamps.

```
mkvgo chapters [-json] <file.mkv|->
```

Pass `-` as the path to read from stdin.

```bash
mkvgo chapters video.mkv
cat video.mkv | mkvgo chapters -
```

### attachments

List attachments (fonts, cover art, etc.) with MIME types and sizes.

```
mkvgo attachments [-json] <file.mkv|.mp4|->
```

MP4 paths are accepted (MP4 has no attachment equivalent, so the list is empty).

Pass `-` as the path to read from stdin.

```bash
mkvgo attachments video.mkv
cat video.mkv | mkvgo attachments -
```

### tags

Show all tags (target type, track associations, key-value pairs).

```
mkvgo tags [-json] <file.mkv|.mp4|->
```

MP4 paths are accepted: the movie-level iTunes tags (`ilst`) are shown as tags.

Pass `-` as the path to read from stdin.

```bash
mkvgo tags video.mkv
cat video.mkv | mkvgo tags -
```

### probe

Full dump of all metadata: info, tracks, chapters, attachments, tags, the keyframe index, and — for MP4 — any dropped (non-carried) tracks such as cover art. Per track it prints the ffprobe-equivalent stream fields read head-only: codec long name, profile/level, pixel format, colour code points, HDR10 static metadata (MaxCLL/MaxFALL + mastering display), Dolby Vision, display rotation, sample/display aspect ratio, frame rate, frame count, per-track duration, bitrate, field order, channel count/layout, sample rate (with the SBR output rate), and bit depth. `-json` carries the same fields, including the derived `codec_long_name` and `channel_layout`.

```
mkvgo probe [-json] <file.mkv|.mp4|->
```

`info`, `tracks`, `chapters` and `probe` accept an MP4/MOV path as well as MKV/WebM (read via the head-only MP4 probe; `probe` additionally builds the keyframe index). Pass `-` to read MKV from stdin.

```bash
mkvgo probe -json video.mkv | jq '.tracks[] | select(.type == "audio")'
mkvgo probe movie.mp4
cat video.mkv | mkvgo probe -
```

### keyframes

List the video track's keyframe timestamps — MKV/WebM from the Cues seek index (head-only), or, when the file carries no Cues, from a complete sequential structural scan of the Segment (every keyframe, equal to `ffprobe -skip_frame nokey`, no demux/decode); MP4 from the sample table. The cut points an `-c copy` segmenter aligns on.

```
mkvgo keyframes [-json] <file.mkv|.mp4>
```

```bash
mkvgo keyframes video.mkv            # HH:MM:SS  <ms>  per line
mkvgo keyframes -json movie.mp4      # [0, 2000, 4000, ...]
```

### validate

Check MKV structure for issues. Reports errors and warnings.

```
mkvgo validate [-json] [-strict] <file.mkv>
```

Exits `0` when no error-severity issue is found (warnings are printed but do
not fail), `1` otherwise — also with `-json` — so it is scriptable. `-strict`
makes warnings fail too.

```bash
mkvgo validate video.mkv && echo "no errors"
mkvgo validate -strict video.mkv && echo "no errors, no warnings"
```

### compare

Diff metadata of two MKV files. Shows added, removed, and changed elements.

```
mkvgo compare [-json] <a.mkv> <b.mkv>
```

Exits `0` when the metadata is identical, `1` when it differs (also with `-json`).

```bash
mkvgo compare original.mkv reencoded.mkv
```

---

## Extraction

### demux

Extract tracks to raw codec streams.

```
mkvgo demux <file.mkv> -o <dir> [-t trackID,...]
```

| Flag | Description |
|---|---|
| `-o` | Output directory (required) |
| `-t` | Comma-separated track IDs to extract (default: all) |

```bash
mkvgo demux video.mkv -o ./streams/
mkvgo demux video.mkv -o ./streams/ -t 1,2
```

### extract-attachment

Extract a single attachment by ID.

```
mkvgo extract-attachment <file.mkv> <attachmentID> -o <outfile>
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

```bash
mkvgo extract-attachment video.mkv 1 -o cover.jpg
```

### extract-subtitle

Extract an embedded text subtitle track as SRT, ASS or WebVTT. SRT/ASS apply to MKV/WebM; WebVTT (`-format vtt`) also works on MP4 (tx3g/wvtt).

```
mkvgo extract-subtitle <file.mkv|.mp4> -t <trackID> -o <out> [-format srt|ass|vtt]
```

| Flag | Description |
|---|---|
| `-t` | Track ID to extract (required) |
| `-o` | Output file path (required) |
| `-format` | Output format: `srt` (default), `ass`, or `vtt` |

```bash
mkvgo extract-subtitle video.mkv -t 3 -o subs.srt
mkvgo extract-subtitle video.mkv -t 3 -o subs.ass -format ass
mkvgo extract-subtitle video.mkv -t 3 -o subs.vtt -format vtt
mkvgo extract-subtitle movie.mp4  -t 3 -o subs.vtt -format vtt
```

### to-vtt

Convert an external subtitle sidecar (`.srt`, `.ass`/`.ssa`, or `.vtt`) to WebVTT.

```
mkvgo to-vtt <subtitle.srt|.ass|.vtt> -o <out.vtt>
```

```bash
mkvgo to-vtt subs.fr.srt -o subs.fr.vtt
```

---

## Editing

### edit

Edit metadata using a JSON patch. Accepts JSON as an argument or `-` for stdin.

```
mkvgo edit <file.mkv> -o <out.mkv> '<json>'
mkvgo edit <file.mkv> -o <out.mkv> -
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

The JSON is a partial `Container` struct. Only fields you include are changed.

```bash
mkvgo edit video.mkv -o out.mkv '{"title":"New Title"}'
cat patch.json | mkvgo edit video.mkv -o out.mkv -
```

### edit-title

Change the container title. Shortcut for `edit` with title JSON.

```
mkvgo edit-title <file.mkv> -o <out.mkv> <title>
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

```bash
mkvgo edit-title video.mkv -o out.mkv "My Video (2024)"
```

### edit-track

Edit properties of a specific track.

```
mkvgo edit-track <file.mkv> -o <out.mkv> -t <id> [-lang x] [-name x] [-default|-no-default] [-forced|-no-forced]
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |
| `-t` | Track ID (required) |
| `-lang` | Set language code (e.g. `eng`, `jpn`) |
| `-name` | Set track name |
| `-default` / `-no-default` | Toggle default flag |
| `-forced` / `-no-forced` | Toggle forced flag |

```bash
mkvgo edit-track video.mkv -o out.mkv -t 2 -lang jpn -name "Japanese" -default
```

### edit-inplace

Edit metadata without rewriting the entire file. Only modifies headers -- instant even on large files.

```
mkvgo edit-inplace <file.mkv> '<json>'
```

```bash
mkvgo edit-inplace video.mkv '{"title":"Quick Fix"}'
```

### remove-track

Remove one or more tracks.

```
mkvgo remove-track <file.mkv> -o <out.mkv> -t <trackID,...>
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |
| `-t` | Comma-separated track IDs to remove (required) |

```bash
mkvgo remove-track video.mkv -o clean.mkv -t 3,4
```

### add-track

Add a track from another MKV file.

```
mkvgo add-track <file.mkv> -o <out.mkv> <source:trackID> [-lang code] [-name text]
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |
| `-lang` | Language code for the new track |
| `-name` | Name for the new track |

```bash
mkvgo add-track video.mkv -o out.mkv commentary.mkv:1 -lang eng -name "Commentary"
```

---

## Assembly

### mux

Combine specific tracks from one or more files into a single MKV.

```
mkvgo mux -o <out.mkv> <file:trackID> [<file:trackID> ...]
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

```bash
mkvgo mux -o output.mkv video.mkv:1 audio_eng.mkv:1 audio_jpn.mkv:1
```

### merge

Combine all tracks from multiple MKV files into one.

```
mkvgo merge -o <out.mkv> <file1.mkv> [<file2.mkv> ...]
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

Metadata policy (first-wins): the output's title, chapters, tags and
attachments come from the **first** input only; the other inputs contribute
tracks, not metadata.

```bash
mkvgo merge -o combined.mkv video.mkv audio.mkv subs.mkv
```

### merge-subtitle

Inject an external SRT or ASS subtitle file into an MKV.

```
mkvgo merge-subtitle <file.mkv> -o <out.mkv> <subtitle> [-format srt|ass] [-lang code] [-name text]
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |
| `-format` | Subtitle format: `srt` or `ass`. Default: detected from the sidecar's extension (`.ass`/`.ssa` → ass, otherwise srt) |
| `-lang` | Language code (e.g. `eng`; default `und`) |
| `-name` | Track name (e.g. `"English"`) |

```bash
mkvgo merge-subtitle video.mkv -o out.mkv subs.srt -lang eng -name "English"
mkvgo merge-subtitle video.mkv -o out.mkv subs.ass -format ass -lang jpn
```

### join

Concatenate multiple MKV files sequentially (same codec/track layout required;
a track-count, codec or codec-configuration mismatch is an error).

```
mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

Metadata policy (first-wins): the output's title, chapters, tags and
attachments come from the **first** file; later files' metadata is not carried.

```bash
mkvgo join -o full.mkv part1.mkv part2.mkv part3.mkv
```

---

## Splitting

### split

Split an MKV by time ranges or by chapters.

```
mkvgo split <file.mkv> -o <dir> [-chapters | -range 0-5000,5000-0]
```

| Flag | Description |
|---|---|
| `-o` | Output directory (required) |
| `-chapters` | Split at chapter boundaries |
| `-range` | Comma-separated `start-end` ranges. Each bound is milliseconds (`300000`), fractional seconds (`90.5`) or a clock time (`5:00`, `01:30:00`, `1:30.5`); `0` = end of file |

Cut policy (keyframe alignment): a segment starts at the first **video
keyframe** at/after its start time (leading audio and mid-GOP video are
dropped so the segment starts decodable), and ends right before the next video
keyframe at/after its end time (the GOP straddling the cut is kept, so
chaining segments loses no frame). A range that contains media but no video
keyframe is an explicit error. Audio-only files cut exactly at the requested
times. Chapters are clipped to each segment's range and rebased to its
timeline.

```bash
# Split by chapters
mkvgo split video.mkv -o chapters/ -chapters

# Split first 5 minutes into its own file
mkvgo split video.mkv -o parts/ -range 0-5:00,5:00-0
```

---

## Indexing

### reindex

Rebuild the seek index (SeekHead + Cues) of an MKV or WebM file. Copies all clusters verbatim and emits a new index derived from their content. Use this on files muxed without a usable seek index to restore fast seeking.

```
mkvgo reindex <input.mkv> <output.mkv>
```

```bash
mkvgo reindex source.mkv reindexed.mkv
```

---

## Remux

### to-mp4

Remux an MKV/WebM file to MP4 without transcoding. Compressed samples are copied verbatim. Supported codecs: H.264/HEVC/AV1/VP9 video; AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio; SRT and WebVTT subtitles (→ tx3g; WebVTT can also be carried natively, see below). Colour/HDR, chapters and B-frame ordering are preserved, along with the movie title (`©nam`), the other global tags (ARTIST/ALBUM/GENRE/… → iTunes `ilst` atoms), per-track names (`hdlr`/`udta/name`) and language — the symmetric counterpart of `from-mp4`.

```
mkvgo to-mp4 [--faststart] [--skip-unsupported] [--flatten-subs] [--webvtt-native] [--mp3-container-delay] <input.mkv> <output.mp4>
```

- `--faststart` writes the `moov` box before `mdat` (one extra pass), for progressive HTTP playback.
- `--skip-unsupported` drops tracks whose codec MP4 cannot carry (e.g. TrueHD) and reports each, instead of failing the whole remux.
- `--flatten-subs` carries ASS/SSA subtitles (which have no native MP4 form) as plain `tx3g` timed text. Lossy — all styling, positioning and karaoke is discarded.
- `--webvtt-native` carries WebVTT as native `wvtt` (ISO/IEC 14496-30) instead of the default `tx3g`. `wvtt` is lossless and read by Apple/Safari/CMAF, but **not** by ffmpeg's MP4 demuxer; leave it off for the widest compatibility.
- `--mp3-container-delay` carries an MP3 track's encoder delay as an edit list (like AAC). **Off by default**, because MP3's delay is already in its in-band Xing/LAME header — a derived edit list over-trims and desyncs a native MKV/WebM MP3. Opt in only to round-trip an MP3 that originated in an MP4 (rare), and pass it to `from-mp4` too.

Subtitles never fail the remux: SRT and WebVTT are carried as `tx3g` by default; a subtitle whose format cannot be carried (e.g. ASS without `--flatten-subs`, or bitmap PGS/VOBSUB) is dropped with a reason.

Cover art IS carried: the first JPEG/PNG image attachment (one named `cover.*` preferred) becomes the iTunes `covr` atom, and `from-mp4` brings it back as an attachment.

Not carried into MP4: **other attachments** (fonts — note an ASS track flattened with `--flatten-subs` loses its attached fonts), **track-targeted tags**, and global tags outside the mapped set (ARTIST, ALBUM, DATE_RELEASED, GENRE, COMMENT, ENCODER, COMPOSER, DESCRIPTION). Nested and untitled chapters are flattened out, and the Nero `chpl` chapter list caps at 255 entries (the QuickTime chapter track carries the full list).

```bash
mkvgo to-mp4 video.mkv video.mp4
mkvgo to-mp4 --faststart --skip-unsupported video.mkv video.mp4
mkvgo to-mp4 --flatten-subs anime.mkv anime.mp4         # ASS → plain tx3g
mkvgo to-mp4 --webvtt-native web.mkv web.mp4            # WebVTT → lossless wvtt (Apple/CMAF)
```

### from-mp4

Remux an MP4 file to MKV. Reads H.264/HEVC/AV1/VP9, AAC/MP3/DTS/Opus/AC-3/E-AC-3, FLAC, tx3g subtitles (→ SRT) and wvtt subtitles (→ WebVTT); colour, chapters, the movie title, the other global tags (ARTIST/ALBUM/…), per-track names and language round-trip back to Matroska (and back out to MP4 with `to-mp4`). Audio decodes bit-identically across the round trip for AAC/AC-3/E-AC-3/FLAC; Opus and MP3 stay in sync (their delay is handled by the decoder from the bitstream). Accepts `--mp3-container-delay` (see `to-mp4`).

```
mkvgo from-mp4 [--mp3-container-delay] <input.mp4> <output.mkv>
```

QuickTime `.mov` files are read too, including the raw-camera/iPhone layout
(`mdat` first, `moov` at the end, QuickTime v1/v2 sound descriptions).

```bash
mkvgo from-mp4 video.mp4 video.mkv
mkvgo from-mp4 iphone_clip.mov clip.mkv
```

### to-webm

Remux an MKV/WebM file to WebM, copying the media verbatim. Only WebM-subset codecs are allowed (VP8/VP9/AV1 video, Vorbis/Opus audio, WebVTT subtitles); a source with any other codec is rejected. Non-WebM elements (chapters, attachments, tags) are dropped (a warning lists the ones actually present). The output is seekable: it carries a Cues index and a SeekHead.

```
mkvgo to-webm <input.mkv> <output.webm>
```

```bash
mkvgo to-webm video.mkv video.webm
```

