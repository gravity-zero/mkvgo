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

**Remote files.** An `http://`/`https://` URL is accepted as the input by the
inspection commands (`info`, `tracks`, `probe`, `keyframes` — and `chapters`/
`tags`/`attachments` on MP4, whose probe is fully head-only) and as the
**source** of `to-mp4`, `from-mp4` and `to-hls`. Reads go through HTTP Range
requests: inspection transfers a few ranged kilobytes whatever the file size;
a remux reads sequentially (a streamed download). The server must honour
`Range` (S3, nginx, caddy… do); one that ignores it gets an explicit error,
never a silent full download. Remote Matroska metadata is head-only
(Info/Tracks/keyframes) — chapters, attachments and tags need the local file.

```bash
mkvgo probe https://nas.local/library/movie.mkv     # a few KB transferred
mkvgo to-mp4 https://nas.local/movie.mkv local.mp4  # remux while downloading
```

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

Check MKV structure for issues. Reports errors and warnings — structural
(TimecodeScale, duplicate track IDs, missing codec data, backwards
timecodes…) and **streaming readiness**: a missing Cues index, cue points
referencing a non-video track (seeking would land on audio — an error),
cue times matching no actual video keyframe (stale index), subtitle blocks
without BlockDuration (cue end times lost), video without DefaultDuration,
AAC without its AudioSpecificConfig. Every finding names the fix
(usually `mkvgo reindex`).

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

### hash

Store each track's content SHA-256 as a `CONTENT_SHA256` tag, making the file
self-verifying (`mkvgo verify`) — bit rot or transfer corruption is detectable
with no external checksum file. Without `-o` the tags are written in place
(instant on mkvgo-written files, which reserve metadata padding; a file with
no room needs `-o` for a full rewrite). Re-hashing replaces the tags.

```
mkvgo hash <file.mkv> [-o <out.mkv>]
```

MP4s are hashed at remux time instead (`to-mp4 --hash`): mkvgo does not
rewrite MP4 metadata in place.

```bash
mkvgo hash archive.mkv                # in place
mkvgo hash video.mkv -o hashed.mkv    # full rewrite
```

### verify

Recompute the per-track content hashes and compare them with the stored
hashes — MKV/WebM: the `CONTENT_SHA256` tags written by `hash`; MP4: the
freeform atoms written by `to-mp4 --hash`. Exits `0` when every hashed track
is intact, `1` on any mismatch; errors when the file was never hashed.

```
mkvgo verify [-json] <file.mkv|.mp4>
```

```bash
mkvgo hash archive.mkv && mkvgo verify archive.mkv && echo intact
```

### compare

Diff metadata of two files. Shows added, removed, and changed elements. Either
side may be an MP4/MOV (read via the head-only MP4 probe), so a remux
round-trip can be verified.

```
mkvgo compare [-json] [-blocks] <a.mkv|.mp4> <b.mkv|.mp4>
```

`-blocks` additionally compares the media CONTENT: per-track block count,
payload byte total and a SHA-256 over the payloads in stream order (MKV/WebM
on both sides). An identical result proves a remux/reindex/split+join
round-trip carried every frame byte-identically.

Exits `0` when identical, `1` when anything differs (also with `-json`).

```bash
mkvgo compare original.mkv reencoded.mkv
mkvgo compare movie.mkv movie.mp4     # verify a to-mp4 round-trip
mkvgo compare -blocks a.mkv b.mkv     # prove the media content is identical
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

### add-attachment

Attach a file (font, cover art, ...). The MIME type is sniffed from the content
(JPEG/PNG/GIF/WebP images, TTF/OTF/WOFF fonts, PDF) unless `-mime` is given.
An attached `cover.jpg`/`cover.png` is what `to-mp4` carries as the MP4 cover art.

```
mkvgo add-attachment <file.mkv> -o <out.mkv> <attachment file> [-name text] [-mime type]
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |
| `-name` | Attachment name (default: the file's base name) |
| `-mime` | MIME type (default: sniffed from content, then extension) |

```bash
mkvgo add-attachment video.mkv -o out.mkv cover.jpg
mkvgo add-attachment video.mkv -o out.mkv font.ttf -name "Subtitle font"
```

### remove-attachment

Remove an attachment by ID or exact name. Fails before writing anything when
no attachment matches.

```
mkvgo remove-attachment <file.mkv> -o <out.mkv> <attachmentID|name>
```

```bash
mkvgo remove-attachment video.mkv -o out.mkv cover.jpg
mkvgo remove-attachment video.mkv -o out.mkv 2
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

mkvgo-written files reserve padding after the metadata, so edits that GROW
the metadata (longer title, added tags/chapters) usually still fit; beyond
the reserve the command fails and a full rewrite (`edit`) is needed. The
head SeekHead is rebuilt in place (its Cues entry preserved), and tags kept
after the clusters (mux statistics) are folded into the head, not duplicated.

```bash
mkvgo edit-inplace video.mkv '{"title":"Quick Fix"}'
```

### set-chapters

Replace a file's chapters from an OGM simple-format text file -- the
`CHAPTER01=...`/`CHAPTER01NAME=...` format mkvmerge (`--chapters`) and ffmpeg
understand. Each chapter ends where the next starts.

```
mkvgo set-chapters <file.mkv> -o <out.mkv> <chapters.txt>
```

```
CHAPTER01=00:00:00.000
CHAPTER01NAME=Intro
CHAPTER02=00:05:12.500
CHAPTER02NAME=Part One
```

```bash
mkvgo set-chapters video.mkv -o out.mkv chapters.txt
```

### extract-chapters

Export a file's chapters in the same OGM format, to stdout or `-o <file>` --
ready for `set-chapters` or `mkvmerge --chapters`. MP4/MOV inputs are accepted.

```
mkvgo extract-chapters <file.mkv|.mp4> [-o <chapters.txt>]
```

```bash
mkvgo extract-chapters video.mkv > chapters.txt
mkvgo extract-chapters movie.mp4 -o chapters.txt
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
mkvgo split <file.mkv> -o <dir> [-chapters | -range 0-5:00,5:00-0 | -every 6:00] [-pattern name]
```

| Flag | Description |
|---|---|
| `-o` | Output directory (required) |
| `-chapters` | Split at chapter boundaries |
| `-range` | Comma-separated `start-end` ranges. Each bound is milliseconds (`300000`), fractional seconds (`90.5`) or a clock time (`5:00`, `01:30:00`, `1:30.5`); `0` = end of file |
| `-every` | Split into keyframe-aligned segments of roughly this duration (same time syntax). Boundaries come from the Cues index -- reindex a file without one first |
| `-pattern` | Output name pattern (default `part_%03d.mkv`; `%d` = part number). With `-chapters`, `{title}` is replaced by the sanitized chapter title (duplicates get a numeric suffix) |

Exactly one of `-chapters`, `-range` or `-every` must be given.

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

# Keyframe-aligned ~6-minute segments (archive / pre-segmentation)
mkvgo split video.mkv -o segments/ -every 6:00

# One file per chapter, named after the chapter titles
mkvgo split video.mkv -o chapters/ -chapters -pattern "{title}.mkv"
```

---

## Indexing

### reindex

Rebuild the seek index (SeekHead + Cues) of an MKV or WebM file. Copies all clusters verbatim and emits a new index derived from their content. Use this on files muxed without a usable seek index to restore fast seeking.

The result is always reopened and its Cues checked against the index built during the copy (a light, millisecond-cost verification). `--deep-verify` additionally runs a full-read validation of the output and a byte-level comparison of the cluster payloads against the source, at the cost of reading both files in full.

```
mkvgo reindex <input.mkv> [output.mkv] [--deep-verify] [--replace] [--keep-backup]
```

- Without `--replace`, `<output.mkv>` is required: the rebuilt file is written there, the source is untouched.
- `--replace` rebuilds `<input.mkv>` in place: no output path is given (supplying one is a usage error). The rebuild happens in a temporary file in the same directory, is verified, and only then atomically replaces the original -- the source is never touched until every check has passed. This needs write permission on the directory (temp file + rename), not just on the file itself.
- `--keep-backup` (only with `--replace`) preserves the pre-op original as `<input.mkv>.bak` instead of discarding it.

```bash
mkvgo reindex source.mkv reindexed.mkv

# Rebuild the index in place, keeping the pre-op file as source.mkv.bak
mkvgo reindex source.mkv --replace --keep-backup --deep-verify
```

---

## Remux

### to-mp4

Remux an MKV/WebM file to MP4 without transcoding. Compressed samples are copied verbatim. Supported codecs: H.264/HEVC/AV1/VP9 video; AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio; SRT and WebVTT subtitles (→ tx3g; WebVTT can also be carried natively, see below). Colour/HDR, chapters and B-frame ordering are preserved, along with the movie title (`©nam`), the other global tags (ARTIST/ALBUM/GENRE/… → iTunes `ilst` atoms), per-track names (`hdlr`/`udta/name`) and language — the symmetric counterpart of `from-mp4`.

```
mkvgo to-mp4 [--faststart] [--skip-unsupported] [--flatten-subs] [--webvtt-native] [--mp3-container-delay] [--hash] <input.mkv> <output.mp4>
```

- `--faststart` writes the `moov` box before `mdat` (one extra pass), for progressive HTTP playback.
- `--skip-unsupported` drops tracks whose codec MP4 cannot carry (e.g. TrueHD) and reports each, instead of failing the whole remux.
- `--flatten-subs` carries ASS/SSA subtitles (which have no native MP4 form) as plain `tx3g` timed text. Lossy — all styling, positioning and karaoke is discarded.
- `--webvtt-native` carries WebVTT as native `wvtt` (ISO/IEC 14496-30) instead of the default `tx3g`. `wvtt` is lossless and read by Apple/Safari/CMAF, but **not** by ffmpeg's MP4 demuxer; leave it off for the widest compatibility.
- `--hash` computes each track's content SHA-256 while the samples stream (no extra I/O) and stores them as freeform `ilst` atoms — the MP4 becomes self-verifying via `mkvgo verify`.
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

> The streaming commands (`to-hls`, `hls-segment`, `to-abr`) are covered end to
> end — modes, sources, ABR, security, trick-play — in the
> **[streaming guide](streaming.md)**. Below is the per-flag reference.

### to-hls

Package a media file — **MKV/WebM or MP4/MOV** (sniffed from the first bytes)
— as a fragmented-MP4 / **CMAF** presentation in an output directory, served through two manifests over the same segments: **HLS** (`master.m3u8` with `BANDWIDTH`/`RESOLUTION`/`CODECS`, plus one media playlist per rendition) and **DASH** (`manifest.mpd`, one AdaptationSet per rendition). Tracks are demuxed — the video rendition (`playlist.m3u8`, `init.mp4`, `seg00001.m4s` …) and one rendition per audio track (`audio1.m3u8`, `init_a1.mp4`, `seg_a1_00001.m4s` …), declared as an `EXT-X-MEDIA` AUDIO group — so multi-audio sources (VF/VO) get native language selection in hls.js/Safari/dash.js. No transcoding — samples are copied verbatim into CMAF fragments, so only the codecs `to-mp4` supports are carried (H.264/HEVC/AV1/VP9 video, AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio). Segments are cut on video keyframes and are independently decodable, so a player can start at any segment. Secondary video tracks are dropped with a reason.

Text subtitle tracks (SRT, WebVTT, ASS/SSA flattened to plain text) ride as segmented **WebVTT renditions** (`subN.m3u8` + `subN_*.vtt`), declared in the master playlist with their language/name/default/forced flags; bitmap subtitles (PGS/VOBSUB) are dropped with a reason. This is the CMAF "copy rung" of an HLS ladder — the packaging, not the encoding: bitrate variants (real adaptive streaming) still require a transcoder.

```
mkvgo to-hls <input.mkv> -o <dir> [-segment 6]
```

| Flag | Description |
|---|---|
| `-o` | Output directory (required) |
| `-segment` | Target segment length in seconds (default 6). Segments are cut on the first video keyframe at/after each multiple |
| `--keep-tracks` | Comma-separated Matroska track IDs to carry (a **Virtual Edit Layer**): serve a "VF only", "VO + English subs" or "clean" version from one source, no copy — just a different track subset. Video is required |

```bash
mkvgo to-hls video.mkv -o stream/
mkvgo to-hls video.mkv -o stream/ -segment 4
mkvgo to-hls multi.mkv -o vf/    --keep-tracks 1,2      # "VF only": video + French audio, no other tracks
mkvgo hls-segment multi.mkv master --keep-tracks 1,2,5  # same subset, on demand
# serve stream/ over HTTP; play stream/master.m3u8 (hls.js/Safari) or stream/manifest.mpd (dash.js)

# AES-128 encryption + signed URLs:
mkvgo to-hls video.mkv -o stream/ --aes-key 00112233445566778899aabbccddeeff \
  --aes-key-uri https://api.example.com/key --url-prefix "https://cdn.example.com/v1/"
```

An **audio-only** source (music, podcasts) packages fine: the first audio
track becomes the primary rendition and boundaries follow its sample grid.
When the source has video, a **trick-play I-frame playlist** (`iframe.m3u8`,
`EXT-X-I-FRAMES-ONLY`) is emitted and declared in the master
(`EXT-X-I-FRAME-STREAM-INF`): one keyframe per segment as a byte range into
the existing segments — zero extra media, what players use for scrubbing
previews. (Not emitted when encrypting; on-demand plans expose it for MP4
sources, whose sample table makes the ranges computable head-only.)

`--single-file` packs each rendition into ONE progressive file (`stream.mp4`,
`stream_a1.mp4` …: init + `sidx` + all fragments) served by byte ranges — the
HLS playlists use `EXT-X-BYTERANGE` and the DASH manifest the on-demand
profile's `SegmentBase`/`indexRange`. Two media files instead of hundreds:
friendlier to object storage; the server only needs `Range` support.
Incompatible with `--aes-key`.

Security flags (shared with `hls-segment`; both must use the same values):

- `--aes-key <32 hex>` + `--aes-key-uri <uri>` — encrypt every media segment
  with AES-128-CBC (whole-segment, PKCS#7, IV = segment sequence, per RFC
  8216) and write the `EXT-X-KEY` line. The key itself is never written to the
  output — serving it (with authentication) is the server's job. Init segments
  and subtitles stay clear. AES-128 is HLS-only: no `manifest.mpd` is emitted
  for an encrypted presentation (DASH uses CENC, not implemented). Supported
  by hls.js; Safari/FairPlay requires SAMPLE-AES (DRM territory, out of
  scope). ffmpeg's own HLS demuxer does not decrypt whole-segment fMP4 —
  verified spec-conformant by openssl round-trip.
- `--url-prefix <prefix>` — prepend a base to every URI the playlists and the
  MPD reference (CDN base, or a token route). The library form is
  `Options.RewriteURL func(name) string`, which can append per-resource signed
  tokens instead.

### hls-segment

Serve one resource of an HLS presentation **on demand** — nothing is
pre-generated. For a Matroska source, `PlanHLS` reads the metadata, Cues and
first/last clusters (a few bounded reads, ranged when the source is a URL);
for an **MP4/MOV source the moov sample table IS the index**, so the plan is
exact by construction — every resource including the master playlist, the
DASH manifest and the I-frame playlist is byte-identical to the full pass.
It then builds
just the requested resource: the master or media playlist, the init segment,
or the N-th media segment (1-based, matching the playlist's `segNNNNN.m4s`).
A media segment is built by seeking straight to its window through the Cues
and reading only that window — first-play latency is milliseconds and storage
cost is zero, whatever the file size.

```
mkvgo hls-segment <input.mkv|url> <master|playlist|init|N> [-o out] [-segment 6]
```

Without `-o` the resource goes to stdout (pipe it from a server handler).
`-segment` must match across calls — it defines the boundaries.

The output is **byte-identical** to the corresponding file `to-hls` writes
(same boundaries, same fragments, cover art and global tags in the init), so
pre-generated and on-demand serving can be mixed transparently. Text subtitle
tracks are declared in the master playlist and served as segmented WebVTT
renditions (`subN.m3u8` + windowed `subN_00001.vtt`…, plus the whole track as
`subN.vtt`); text blocks have no cue index, so the cues come from scanning
the clusters — incremental, bounded and cached: a windowed request costs one
bounded read whether playback is sequential or seeking, only the whole-track
`subN.vtt` costs a full pass. The remaining difference from `to-hls`: the master
playlist's `BANDWIDTH` is estimated from the source's cluster sizes. The
source must carry a Cues index (`mkvgo reindex` adds one).

```bash
mkvgo hls-segment movie.mkv master                 # multivariant playlist → stdout
mkvgo hls-segment movie.mkv 42 -o seg00042.m4s     # just that segment
mkvgo hls-segment movie.mkv seg00042.m4s           # same, by player-facing name
mkvgo hls-segment movie.mkv sub1.vtt               # a subtitle rendition
mkvgo hls-segment https://nas/movie.mkv 3          # remote: reads only segment 3's ranges
```

### to-abr

Package several **pre-encoded quality variants** of the same content into one
multi-variant HLS presentation — "ABR light": mkvgo does the packaging (no
transcoding; producing the encodes remains a transcoder's job). The first
source is the reference: its audio tracks and subtitles serve every variant;
the other sources contribute only their video rendition (packaged
`VideoOnly`). Each source lands in `v1/`, `v2/`, … and the top `master.m3u8`
declares one variant per source with its real `BANDWIDTH`/`RESOLUTION`/
`CODECS` over the shared audio/subtitle groups.

```
mkvgo to-abr -o <dir> <best.mkv> <lower.mkv> [...] [-segment 6] [--aes-key … --aes-key-uri …] [--url-prefix …]
```

For seamless switching the sources should share the keyframe cadence (same
GOP length, keyframes at the same times — encode each variant with a fixed GOP
and forced keyframes). The combined **HLS** master always plays; a mismatched
cadence still switches, just realigning on the next keyframe.

A combined **DASH** `manifest.mpd` (one video AdaptationSet, a Representation
per variant) is written at the top level **only when the variants are
segment-aligned** — DASH shares one SegmentTimeline across a switch set, so it
is unsafe otherwise. When they are not aligned, only each variant's own
`v{k}/manifest.mpd` is written.

```bash
mkvgo to-abr -o stream/ movie-1080p.mkv movie-720p.mkv movie-480p.mkv
```

The variants may be Matroska/WebM **or** progressive/fragmented (CMAF) MP4 — a
pre-encoded ladder that is already fragmented MP4 is read directly.

### abr-segment

Serve one resource of the multi-variant presentation **on demand** — the
on-demand counterpart of `to-abr` (as `hls-segment` is to `to-hls`). Nothing is
pre-generated: each resource is built when requested, and a remote variant
(httpfs) transfers only the ranges a viewer watches. The first argument is the
resource name (`master.m3u8`, or `v{k}/<name>` such as `v2/seg00042.m4s`), the
rest are the quality variants best first.

```
mkvgo abr-segment <master.m3u8|v{k}/name> <best> <lower> [...] [-o out] [-segment 6]
```

```bash
mkvgo abr-segment master.m3u8 movie-1080p.mp4 movie-720p.mp4     # top manifest
mkvgo abr-segment v2/seg00042.m4s 1080p.mp4 720p.mp4 -o s.m4s    # one segment
```

Every `v{k}/<name>` is byte-identical to the file `to-abr` would have written.
The library equivalent is `mp4.PlanABR` (see library.md).

### extract-frame

Extract the video keyframe nearest a timestamp, **decoder-ready** — the mkvgo
half of a thumbnail/storyboard pipeline. The keyframe is seeked through the
Cues (a few bounded reads, no scan) and packed so a decoder ingests it
directly: Annex-B with the SPS/PPS (H.264) or VPS/SPS/PPS (HEVC) prepended, or
a minimal IVF wrapper for VP8/VP9/AV1. mkvgo never decodes — turning the
sample into an image is one ffmpeg call away.

```
mkvgo extract-frame <file.mkv> <time> -o <out.h264|.hevc|.ivf>
```

```bash
mkvgo extract-frame movie.mkv 00:12:30 -o frame.h264
ffmpeg -i frame.h264 -frames:v 1 thumb.jpg

# storyboard: one thumbnail per keyframe
for t in $(mkvgo keyframes -json movie.mkv | jq -r '.[]'); do
  mkvgo extract-frame movie.mkv ${t}ms -o f.h264 -f && ffmpeg -y -i f.h264 -frames:v 1 -vf scale=160:-1 thumb_${t}.jpg
done
```
