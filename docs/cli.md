# CLI Reference

The complete command/flag/output reference. New here? The
**[recipes](recipes.md)** are a gentler, task-first starting point.

```
mkvgo <command> [options]
```

Global flags:
- `-json` -- structured JSON output (info, tracks, chapters, attachments, tags, probe, keyframes, validate, compare, analyze; accepted but ignored by writing commands)
- `-f`, `--force` -- overwrite an existing output file. Without it, every command that writes a new file refuses to clobber an existing one (`out.mkv already exists`). `edit-inplace` is the exception: it modifies its input file by design.
- `--version` -- print version and exit
- `-h`, `--help` -- show help for a command

Exit codes: `0` success, `1` any error (bad usage, unreadable input, failed
operation). `validate` also exits `1` when error-severity issues are found
(`-strict` makes warnings fail too) and `compare` when the files differ, so
they can gate scripts (`mkvgo validate f.mkv && ...`).

**Remote files.** An `http://`/`https://` URL or an `s3://bucket/key` reference
is accepted as the input by the inspection commands (`info`, `tracks`,
`probe`, `keyframes`, `analyze` -- and `chapters`/`tags`/`attachments` on MP4, whose probe
is fully head-only) and as the **source** of `to-mp4`, `from-mp4` and
`to-hls`. Reads go through ranged requests: inspection transfers a few ranged
kilobytes whatever the file size; a remux reads sequentially (a streamed
download). The server must honour `Range` (S3, nginx, caddy… do); one that
ignores it gets an explicit error, never a silent full download. Remote
Matroska metadata is head-only (Info/Tracks/keyframes) -- chapters,
attachments and tags need the local file.

`s3://` sources are signed with AWS Signature Version 4 (stdlib
`crypto/hmac`/`crypto/sha256` only, no SDK). Credentials, region and endpoint
come from the standard AWS environment variables:
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`,
`AWS_REGION` (or `AWS_DEFAULT_REGION`, defaulting to `us-east-1` if neither is
set), and `AWS_ENDPOINT_URL` for S3-compatible services (path-style vs
virtual-hosted-style is a library-only option -- see `docs/library.md`).

```bash
mkvgo probe https://nas.local/library/movie.mkv     # a few KB transferred
mkvgo to-mp4 https://nas.local/movie.mkv local.mp4  # remux while downloading
AWS_REGION=eu-west-1 mkvgo probe s3://my-bucket/movies/one.mkv
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

Full dump of all metadata: info, tracks, chapters, attachments, tags, the keyframe index, and - for MP4 - any dropped (non-carried) tracks such as cover art. Per track it prints the standard-prober stream fields read head-only: codec long name, profile/level, pixel format, colour code points, HDR10 static metadata (MaxCLL/MaxFALL + mastering display), Dolby Vision, display rotation, sample/display aspect ratio, frame rate, frame count, per-track duration, bitrate, field order, channel count/layout, sample rate (with the SBR output rate), and bit depth. `-json` carries the same fields plus every derived string as its own key, so a scanner consumes the shape directly with no post-processing: `codec_long_name`, `channel_layout`, `avg_frame_rate`, `sample_aspect_ratio`/`display_aspect_ratio`, the colour code points as conventional names (`color_space_name` "bt2020nc", `color_transfer_name` "smpte2084", `color_primaries_name`, `color_range_name`), `stereo_mode_name`, `resolved_language` (BCP-47 when present, else the legacy tag), `effective_sample_rate` (the decoder's rate, SBR applied), and **`hdr_format`** - the one-word dynamic-range classification a tonemap-or-direct-play decision keys on: `dolby-vision` | `hdr10` | `hlg` | `sdr` (absent when unknown). Dolby Vision profile 8 (the cross-compatible flavour) classifies by its BASE layer - `bl_signal_compatibility_id` 1/2/4 → `hdr10`/`sdr`/`hlg`, since that layer plays without a DoVi decoder; only a stream that genuinely needs the DoVi rendering path reports `dolby-vision`, and the raw `dolby_vision` fields ride alongside for consumers applying their own policy. Both MKV/WebM and MP4/MOV, one JSON shape.

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

List the video track's keyframe timestamps - MKV/WebM from the Cues seek index (head-only), or, when the file carries no Cues, from a complete sequential structural scan of the Segment (every keyframe, from a structural scan, no demux/decode); MP4 from the sample table. The cut points an `-c copy` segmenter aligns on.

```
mkvgo keyframes [-json] <file.mkv|.mp4>
```

```bash
mkvgo keyframes video.mkv            # HH:MM:SS  <ms>  per line
mkvgo keyframes -json movie.mp4      # [0, 2000, 4000, ...]
```

### analyze

Stream statistics: per-track exact frame/keyframe counts (lacing expanded), byte totals, average/peak bitrate, GOP spans, a declared-vs-true duration reconciliation, and a per-video-track constant/variable frame rate (cfr/vfr) classification - computed from block HEADERS alone (track, timecode, keyframe flag, byte size, duration), never a decoded sample. The walk is head-only: cost is proportional to the block-header count, not the media volume. Matroska/WebM only for now; MP4 is a follow-up.

The cfr/vfr column comes from the deltas between consecutively PRESENTED video frame timecodes - a bounded window restores presentation order first, since blocks are stored in decode order and B-frame reordering would otherwise read as duration variance - compared against the modal (most common) delta with +-1ms slack (Matroska timecodes are millisecond-scale, so a constant 23.976fps track legitimately alternates 41ms/42ms deltas). Only when more than 1% of the deltas (and at least 2) fall outside that slack is the track reported variable, with a warning (some pipelines assume constant frame rate) - isolated dropped-frame holes or splices on an otherwise constant-rate track stay constant instead of flipping the whole title to vfr.

```
mkvgo analyze [-json] <file.mkv|url>
```

```bash
mkvgo analyze video.mkv
# Duration: 00:14:23 (declared 00:14:23)
# Overall bitrate: 4213 kb/s
# Clusters: 431, blocks: 25920
#
# Track 1 (video, h264): 20736 frames (20736 packets), 173 keyframes, 41821440 bytes
#   avg 3877 kb/s, peak 5210 kb/s, duration 00:14:23, 24.000 fps cfr
#   GOP: min 120, max 120, avg 120.0 frames; keyframe every ~5000ms (max 5000ms at 00:31:15); reordered=true
#
# Track 2 (audio, aac): 40448 frames (10112 packets), 40448 keyframes, 6469120 bytes
#   avg 60 kb/s, peak 62 kb/s, duration 00:14:23, 46.865 fps

mkvgo analyze -json video.mkv    # AnalyzeReport
```

### fingerprint

Container-independent content identity: a `Presentation` hash over every track's payload content, plus one SHA-256 digest per track (decode order) - the same digest `compare -blocks` uses to prove a round-trip byte-identical. Two files carrying the same audio/video/subtitle streams fingerprint identically even with different container metadata (title, muxing app), a different track order, or a different container (Matroska/WebM vs MP4/MOV) - the use case is cross-container dedup in a media library. Unlike `analyze` this is a FULL read: every track's frame payload is read and hashed. MP4/MOV sources are hashed by remuxing to a temporary Matroska file first (sample bytes are copied verbatim), so an MP4 and an MKV carrying the same encode fingerprint identically.

```
mkvgo fingerprint [-json] <file.mkv|.mp4|url>
```

```bash
mkvgo fingerprint video.mkv
# Presentation: 7e1c9a4f...
#   track 1 (video, h264): 9f2b7e...
#   track 2 (audio, aac): 3a0d5c...

mkvgo fingerprint -json video.mkv    # FingerprintReport
```

### validate

Check MKV structure for issues. Reports errors and warnings - structural
(TimecodeScale, duplicate track IDs, missing codec data, backwards
timecodes…) and **streaming readiness**: a missing Cues index, cue points
referencing a non-video track (seeking would land on audio - an error),
cue times matching no actual video keyframe (stale index), subtitle blocks
without BlockDuration (cue end times lost), video without DefaultDuration,
AAC without its AudioSpecificConfig. Every finding names the fix
(usually `mkvgo reindex`).

```
mkvgo validate [-json] [-strict] <file.mkv>
```

Exits `0` when no error-severity issue is found (warnings are printed but do
not fail), `1` otherwise - also with `-json` - so it is scriptable. `-strict`
makes warnings fail too.

```bash
mkvgo validate video.mkv && echo "no errors"
mkvgo validate -strict video.mkv && echo "no errors, no warnings"
```

### hash

Store each track's content SHA-256 as a `CONTENT_SHA256` tag, making the file
self-verifying (`mkvgo verify`) - bit rot or transfer corruption is detectable
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
hashes - MKV/WebM: the `CONTENT_SHA256` tags written by `hash`; MP4: the
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
mkvgo compare [-json] [-blocks] <a.mkv|.mp4> <b.mkv|.mp4> [part2.mkv ...]
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


Several files on the right compare the left one against their **concatenation**,
in order - proving a split kept everything without rebuilding the joined file:

```bash
mkvgo compare -blocks film.mkv part1.mkv part2.mkv part3.mkv
# identical content (3 parts) + exit 0  =>  the parts hold exactly the film's content
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
`CHAPTER01=...`/`CHAPTER01NAME=...` format chapter-aware tools
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
ready for `set-chapters` and other chapter-aware tools. MP4/MOV inputs are accepted.

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

Commands whose output differs from their source - `remove-track`, `add-track`,
`merge-subtitle`, `merge-ass` - write their own SegmentUID (derived, so the
same command writes the same bytes twice) instead of copying the source's: two
different files must not claim to be one segment. Hard links (PrevUID/NextUID)
are kept - the timeline has not moved. `to-webm` writes no segment identity at
all: the WebM subset supports none.

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

Metadata policy: the output's title and tags come from the **first** file
(first-wins). Attachments are pooled from every file instead, identified by
content so a font attached to one part only is not lost and a font repeated in
every part lands once; cover art stays single.

Per-track statistics (`BPS`, `DURATION`, `NUMBER_OF_FRAMES`, `NUMBER_OF_BYTES`)
are measured on what is actually written and always attached. A content hash is
re-measured too when the source carried one, so `mkvgo verify` on a joined file
passes instead of reporting the first part's checksum as a mismatch.

Chapters are the exception, because they describe the timeline rather than
decorate it: **every** file contributes its own, shifted by the offset its
blocks were actually written at. Splitting on chapters and joining the parts
back gives every chapter again, each at the instant it was cut on. Repeated
ChapterUIDs are renumbered - except between parts of one timeline, where a
repeat is that chapter still running across the cut and is dropped rather than
announced twice. Chapters linked to another segment are dropped.

Where the seam falls depends on what the files are to each other. Two files
that merely follow one another resume after everything the previous one holds,
its audio tail included - the last frame's measured end, never the declared
duration, so a container that declares more than it holds opens no gap. Parts
of one timeline, which `split` chains through their segment identity
(`PrevUID`/`NextUID`), resume where the **picture** does instead: a cut runs
down the file at a video keyframe, so the part before it keeps sound from after
the cut and the part after it opens on sound from before it, and putting the
seam past that overlap would push the film a little further out at every join.
Parts joined out of order, or with one missing, are not a chain and get the
ordinary seam.

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
times.

A segment's timeline starts at the first frame it actually keeps, not at the
time it was asked for: those differ by however far the source's next keyframe
was, and counting from the requested time would open the segment on a hole of
that length. Its chapters are clipped to its range and measured from the same
first frame, and the duration it declares is the stretch it holds - a little
more than the range, since it keeps the GOP straddling its end.

Its chapters are decided on that same first frame rather than on the bound the
part was asked for: a marker between the two names a frame the PREVIOUS part
holds, so that is the part that carries it. A part still opens on the chapter
that was playing when it starts.

Each part is a segment in its own right and gets its own SegmentUID, derived
from the source so that splitting the same file twice writes the same bytes.
Consecutive parts are chained through `PrevUID`/`NextUID`, which is what lets
`join` recognise them as slices of one timeline and put the seam back exactly
where the cut was.

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

### cue-health

Head-only triage of the seek index: can it actually seek video? `validate` proves cue times against real keyframes but reads the whole file; `cue-health` answers the earlier, cheaper question a library scan needs - from the Tracks and Cues alone, in milliseconds. It spots the dormant defect where a video file's index exists and is non-empty, so everything reports the file as "indexed", yet not one cue keys on video and every seek lands mid-GOP - and the index so coarse that a seek lands nowhere near its target.

```
mkvgo cue-health <file.mkv> [-json]
```

- Exit 0 when healthy, 1 when not (scriptable, like `validate`); the reason names the remedy (`mkvgo reindex`).
- The verdict judges the VIDEO cues, because they are what seeking uses (the keyframe index drops the rest). Cues on an audio track are inert - real muxers routinely cue every track, leaving an index that is mostly "non-video" and seeks perfectly - so their share is reported, never held against a file.
- A video file is unhealthy when its index cannot serve a seek: no cues at all, cues referencing tracks that do not exist (a stale index), not one video cue (the index seeks the audio), or video cues leaving a hole wider than 30s (a seek into the hole lands that far from its target; the reason says where - a tail, measured to the PICTURE's end from the track's statistics `DURATION` tag rather than to the declared duration, which is usually an audio track outlasting the picture, or a hole in the middle). When the track's statistics show the picture missing where the hole is (frames short of the duration at the frame rate), the reason says so: no index can close it. `-probe` looks inside each hole (one bounded, header-only walk over the clusters between the two cues around it) and prints what it holds: uncued keyframes (a reindex closes it), frames without a keyframe (only a re-encode makes it seekable), or a stretch without any video block (the picture is missing from the stream) - the same pass `diagnose` runs on its own. An audio-only file legitimately cues audio.

```bash
mkvgo cue-health movie.mkv
# movie.mkv: 40219 cue(s) (3333 video, 36886 non-video, 0 unknown-track), 0:00:00 to 1:15:02
#   video coverage: worst hole 0:00:02
#   index healthy

mkvgo cue-health broken.mkv
# broken.mkv: 4210 cue(s) (0 video, 4210 non-video, 0 unknown-track), 0:00:00 to 2:01:14
#   index UNHEALTHY: the index keys on non-video tracks only (4210 cue(s), no video cue) - every seek lands mid-GOP: run mkvgo reindex
```

The library equivalent is `ops.CueHealth` / `matroska.CueHealth` (see library.md).


### track-ends

Where each track's content REALLY ends. The declared duration is only the longest track's end - on real files an audio track's - and says nothing about the others: an audio track that dies minutes before the picture leaves a structurally healthy file (index fine, sizes coherent) whose playlists promise audio segments that can never exist. Content is measured against content, one track against another; the declared duration is only a ceiling.

```
mkvgo track-ends <file.mkv> [-json]
```

- Two stages, cheapest first. STATISTICS: the per-track `DURATION` tag mkvmerge and mkvgo write, trusted only when it describes THIS file (same writing application and date as the file, not past the declared duration - the checks mkvmerge applies, since a tag copied through a remux certifies frames the file no longer holds); one head-only read settles every track so stamped. TAIL WALK: for the rest, from a cue 120 s before the end (900 s when a track is still unseen), the final clusters are walked header-only - payloads skipped by size, constant memory - keeping each track's last block. A track silent through the widest window ended at or before its start and is reported so ("at or before"); a file with no index is walked whole, header-only.
- Prints where the picture ends and how far any audio track stops before it. `diagnose` runs the same pass and raises `audio-short` past 5 s.

```bash
mkvgo track-ends episode.mkv
# episode.mkv: declared duration 00:48:52
#   track 1 (video): ends 00:48:10 (statistics)
#   track 2 (audio): ends 00:48:52 (statistics)
#   track 3 (subtitle): ends 00:45:05 (statistics)
#   picture ends 00:48:10
```

### diagnose

One-call triage: classifies a file and names the remedy for every defect, so a library scan can route each file straight to the right repair without stacking separate probes. It composes the seek-index triage (`cue-health`), the per-track audio start delays (the repack defect where audio content starts late), and a declared-size coherence check; the full tolerant walk (`salvage --dry-run`) runs ONLY when the declared Segment size and the real file size disagree - the head-visible signature of truncation or trailing junk. On a healthy file the whole call costs the head plus the first cluster(s).

```
mkvgo diagnose <file.mkv> [-json]
```

- Exit 0 when healthy, 1 when findings are present (scriptable, like `validate`).
- Finding kinds: `no-index`, `index-misskeyed` (not one video cue), `index-sparse` (video cues too far apart to seek into - the detail names each hole with what a bounded probe found inside it; the remedy is the reindex when a hole holds uncued keyframes, re-acquiring the source when the picture is missing there, re-encoding when the stretch has no keyframe), `index-stale-tracks`, `audio-delay` (per track, with the exact `retime` invocation), `truncated` (source incomplete: recovered X of Y declared bytes - re-download; no tool can restore the tail), `picture-missing` (a stretch the hole probe found without any video block - the picture freezes there whatever the index does; located; re-acquire the source), `audio-short` (an audio track ends more than 5 s before the picture - measured content against content by `track-ends`, a lower bound when the track was silent through the whole tail walked; re-acquire the source, playback pads it with silence), `damaged` (repairable: `reindex --resync`), `trailing-junk` (surplus bytes past the declared Segment end - benign, a rewrite drops them; never conflated with `truncated`), `streamed-size` (unsealed Segment), `wrong-container` (the content is another container behind this extension - rename or remux; the file is classified once instead of erroring on every scan pass).
- The JSON output carries the full `cue_health` report, every audio track's `audio_delays_ns` (threshold or not), and the `damage` map when the walk ran.

```bash
mkvgo diagnose movie.mkv
# movie.mkv: 2 finding(s)
#   [index-misskeyed] the index keys on non-video tracks only (4210 cue(s), no video cue) - every seek lands mid-GOP: run mkvgo reindex
#       remedy: mkvgo reindex
#   [audio-delay] audio track 2 starts 900ms after the video
#       remedy: mkvgo retime --shift 2=-900
```

**MP4/MOV sources** (sniffed from the first bytes) run the head-only MP4 triage instead: the top-level box layout tells `truncated` (a declared box overruns the real end of file - present X of Y declared bytes) and `trailing-junk` apart, `no-moov` means nothing any tool can rebuild (re-download), and each track's edit list carries its presentation delay - the `audio-delay` finding then names the exact `retime` invocation, which works on MP4 too. No index triage (the sample table IS the index by construction) and no walk at all: the whole call is head-only. The JSON shape is identical, so one scan loop covers a mixed library.

The library equivalents are `ops.Diagnose` / `matroska.Diagnose` (Matroska), `mp4.Diagnose` (MP4), and the container-agnostic `mkvgo.Diagnose` (root package) which routes like this command; the per-track delay probe alone is `ops.AudioStartDelays` (see library.md).

### reindex

Rebuild the seek index (SeekHead + Cues) of an MKV or WebM file. Copies all clusters verbatim and emits a new index derived from their content. Use this on files muxed without a usable seek index to restore fast seeking.

The result is always reopened and its Cues checked against the index built during the copy (a light, millisecond-cost verification). `--deep-verify` additionally runs a full-read validation of the output and a byte-level comparison of the cluster payloads against the source, at the cost of reading both files in full.

The deep validation compares the result against the source and refuses only when the operation ADDED an error: defects the file already carried (a heritage muxer's mis-keyed cues, subtitles without durations) are printed as `preexisting issue (not from this operation)` with their remedy, without blocking a correct repair. `--strict` restores the absolute behavior - any error-severity issue in the result refuses. This applies to `reindex`, `reindex-inplace` and `retime` alike.

```
mkvgo reindex <input.mkv> [output.mkv] [--deep-verify] [--replace] [--keep-backup] [--resync]
```

- Without `--replace`, `<output.mkv>` is required: the rebuilt file is written there, the source is untouched.
- `--replace` rebuilds `<input.mkv>` in place: no output path is given (supplying one is a usage error). The rebuild happens in a temporary file in the same directory, is verified, and only then atomically replaces the original -- the source is never touched until every check has passed. This needs write permission on the directory (temp file + rename), not just on the file itself.
- `--keep-backup` (only with `--replace`) preserves the pre-op original as `<input.mkv>.bak` instead of discarding it.
- Trailing junk needs no flag: a few unparseable bytes past the declared Segment end (zero padding from batch tools, a crashed in-place journal) are dropped from the rewrite and printed (`dropped N B of trailing junk...`), after a bounded scan proved they carry no trace of a Cluster - not even the bare Cluster ID of one cut mid-write. Real media behind an undershooting declared size keeps being copied, and any hint of media in the trailing bytes keeps the strict refusal below.
- `--resync` (opt-in; works with and without `--replace`) tolerates corrupted regions in the cluster stream -- an element header that does not decode, a declared size that does not match the element's real extent, damaged bytes inside a cluster body, raw junk between clusters. Instead of refusing the file, the walk repairs surgically: a lying size over an intact payload is corrected with zero loss, and damage inside a cluster is cut around (the valid blocks on both sides are kept, chain-validated against the file's real track numbers and timecodes) rather than dropping the whole cluster. Only what cannot be recovered is skipped. Every repaired region and every dropped range is printed (byte offsets + approximate presentation time), so the operator knows exactly what happened -- the loss is usually a few KB, a fraction of a second of one track. The repair is still refused when no valid resume point is found within the scan window (64 MiB), when no cluster survives, or when more than half of the walked payload would be dropped: a mostly-damaged file must not silently "repair" into a stub (use `mkvgo salvage` for best-effort recovery without that cap). Without the flag the strict refusal stays the contract, and the refusal message points at `--resync` when a corrupted region is what stopped it.
- `--clean-cut` (only with `--resync`) resumes video after each damage gap at the next video keyframe instead of the first recovered frame: post-gap P/B frames reference lost pictures and decode with artifacts until the keyframe. Audio resumes immediately (its frames are independent). Preview the cost with `mkvgo salvage --dry-run --clean-cut`.
- `--rollback-delta <file>` writes the inverse delta alongside the repair: the recipe to reconstruct the pre-repair original from the repaired output (see the `rollback` command), typically well under 0.1% of the source size where a backup copy would be the whole file. With the flag set, a repair whose delta cannot be written fails rather than leaving you without the safety net.

```bash
mkvgo reindex source.mkv reindexed.mkv

# Rebuild the index in place, keeping the pre-op file as source.mkv.bak
mkvgo reindex source.mkv --replace --keep-backup --deep-verify

# A file that plays fine but is refused by reindex (players resynchronize
# over its damaged region): repair it, report what was kept and lost
mkvgo reindex source.mkv repaired.mkv --resync
```

```
reindexed source.mkv → repaired.mkv
  repaired range 1: offset 5514-982060, 949.8 KB of media kept that a plain resync would have dropped
  skipped range 1: offset 295912-299899 (3.9 KB), approx 00:00:00-00:00:02
  3.9 KB of corrupted data skipped across 1 range(s)
```

### reindex-inplace

Rebuild the seek index by patching the file itself: the new Cues element is appended inside the Segment, the head SeekHead repointed and any stale Cues voided. Cluster bytes are never moved, no copy of the file is created -- write permission on the file is enough (no temp file in the directory), and no transient disk space is used beyond the new index itself. On large files this takes seconds where a copy takes minutes.

The operation is crash-safe: the bytes about to be overwritten are journaled inside the file (fsynced) before any patch, the result is verified while a rollback is still possible (light check always, full-read validation with `--deep-verify`), and the journal is removed only after the checks pass. Any failure restores the original bytes automatically; a run interrupted by a crash is repaired automatically by the next run, or explicitly with `--rollback`.

```
mkvgo reindex-inplace <file.mkv> [--deep-verify] [--rollback]
```

- `--deep-verify` runs the full-read validation (every cue checked against real video keyframes) before committing the patch; a failure rolls the patch back.
- `--rollback` only restores a file left mid-operation by a crash (no reindexing); it reports when the file carries no journal.
- Streamed files (unknown-size clusters), truncated files, and files with no head SeekHead or Void to hold the rebuilt SeekHead are refused with an explicit message -- use `mkvgo reindex` (copy) for those.
- Once the command has succeeded there is no undo: the journal exists only during the operation. Use `reindex --replace --keep-backup` when you want to keep the pre-op file.

```bash
mkvgo reindex-inplace movie.mkv --deep-verify

# After a crash mid-operation: restore the original bytes
mkvgo reindex-inplace movie.mkv --rollback
```

### retime

Cancel a constant A/V desync - the repack defect where audio content starts N ms after the video - by shifting the block timecodes of the given tracks in place. Matroska block timecodes are relative to their cluster (signed 16-bit, a range of +-32.7 s at the standard 1 ms timecode scale), so cancelling a delay of hundreds of ms is a 2-byte patch per block: no payload byte moves, no rewrite, no temp file, no disk duplication. Cluster CRC-32 elements covering patched blocks are recomputed, and cues keyed on shifted tracks move by the same amount.

```
mkvgo retime <file.mkv|.mp4> --shift <track>=<ms> [--shift <track>=<ms> ...] [--in-place | --replace] [--keep-backup] [--deep-verify] [--strict] [--rollback-delta <file>]
```

**MP4/MOV sources route to the container's native mechanism** (sniffed from the first bytes, never the name): the same flag and sign edit the track's presentation through its edit list (`edts`/`elst`) in the moov - no block is touched at all, so the repair is a few bytes whatever the file size. The write needs permission on the file alone and is crash-ordered on every layout: the new moov is appended and synced to disk FIRST, then the old one is retired to a `free` box with a single 4-byte type flip - at every instant the file carries exactly one intact, authoritative moov, so an interrupted run keeps the original semantics. The retired moov stays behind as a `free` block (bounded, harmless); a faststart source is no longer faststart afterwards (the live moov sits at the tail) - re-run `to-mp4 --faststart` if progressive HTTP serving needs it back. Track and movie durations follow the shift. The Matroska mode flags (`--in-place`/`--replace`/`--keep-backup`/`--deep-verify`/`--strict`/`--rollback-delta`) do not apply and are refused. Explicit refusals: presenting a track before the presentation start (MP4 cannot without trimming media), unknown tracks, fragmented MP4. Library forms: `mp4.RetimeTracks` (same signature as `matroska.RetimeTracks`), or the container-agnostic `mkvgo.RetimeTracks` (root package) which sniffs the first bytes and routes exactly like this command.

Two engines, picked automatically: the **in-place patch** (2 bytes per block under the crash-safe journal, no rewrite, file-only permission) when patches are few relative to the file - a short file, laced audio; the **sequential rewrite** (`--replace` semantics: verified copy, atomic swap, `--keep-backup` keeps the original, directory permission) when they are dense - a multi-track movie, where each 2-byte patch dirties a whole page and in-place I/O grows past a full rewrite. The rewrite also rebuilds the seek index from the shifted blocks (healthy, video-keyed cues even when the source's were not) and handles streamed (unknown-size) Segments that in-place refuses. Force either engine with `--in-place` / `--replace`.

- `--shift 2=-900` moves track 2's blocks 900 ms earlier (positive values move later). Repeat the flag to fix several tracks with different delays in one pass.
- The patches run under the same crash-safe in-file journal as `reindex-inplace`: any failed check rolls the file back byte-identical, and a run interrupted by a crash is repaired automatically by the next in-place operation (or `reindex-inplace --rollback`).
- `--deep-verify` re-walks the whole file afterwards and checks every shifted track's first block moved by exactly the requested amount, on top of the always-on patch verification.
- `--rollback-delta <file>` captures the patches as a rollback delta of a few hundred bytes (see the `rollback` command).
- Refused with an explicit message: a shift finer than the file's timecode resolution, a resulting relative timecode outside int16, an absolute timestamp that would go negative, an unknown track or one with no blocks, a cue mixing shifted and unshifted tracks, and streamed (unknown-size) files - repair those with `reindex` first. Each refusal also wraps a typed sentinel for library callers (`matroska.ErrShiftOutOfRange`, `ErrUnknownTrack`, ... - see library.md): all of them permanent for the same call, none worth retrying.
- Trailing junk past the declared Segment end never blocks the rewrite engine: it is dropped and printed (`dropped N B of trailing junk...`), exactly as `reindex` does. The in-place engine is bounded by the declared end and never even reads it (the junk then stays in the file; `reindex` removes it).

```bash
# Audio (track 2) starts 900 ms late: pull it back into sync, keep an undo delta
mkvgo retime movie.mkv --shift 2=-900 --deep-verify --rollback-delta movie.rbd
```

### salvage

Best-effort recovery copy of a damaged MKV or WebM file. `reindex` and `validate` refuse mid-file corruption by design; `salvage` is the explicitly lossy-tolerant counterpart: metadata elements and cluster payloads are copied verbatim and the Cues index is rebuilt, exactly like `reindex`, but a structural failure inside the cluster stream (a corrupt element header, a zeroed region, a size that overflows) is not fatal. The damaged region is repaired surgically when the bytes allow it -- a lying size field over an intact payload is corrected with zero loss, valid blocks around a gap inside a cluster are kept (chain-validated) -- and only what cannot be recovered is skipped and reported. A truncated source yields one damaged range running to EOF; a clean source yields zero damaged ranges and a result equivalent to `reindex`.

```
mkvgo salvage <in.mkv> <out.mkv> [--json] [--clean-cut]
mkvgo salvage <in.mkv> --dry-run [--json] [--clean-cut]
```

- `--dry-run` maps the damage without writing anything: the report printed is the one the real salvage would produce (repaired ranges, damaged ranges, clean-cut cost), so the decision to repair can be made with the numbers in hand.
- `--clean-cut` resumes video after each damage gap at the next video keyframe (audio resumes immediately); the dropped video bytes are reported.
- `--json` prints the report (`SalvageReport`) as JSON instead of the human summary.
- Never in-place: `<out.mkv>` is always a separate file.
- Exit contract: exit 0 whenever an output file was written (or the dry-run completed), damage or not. Exit 1 with no output only on a hard failure -- the bounded resync scan giving up without reaching a valid resume point or real EOF (garbage longer than the internal cap), or a genuine I/O error.

```bash
mkvgo salvage damaged.mkv --dry-run
mkvgo salvage damaged.mkv recovered.mkv
```

```
salvaged damaged.mkv -> recovered.mkv
  905 cluster(s) copied, 893.1 MB recovered
  repaired range 1: offset 5514-982060, 949.8 KB of media kept that a plain resync would have dropped
  damaged range 1: offset 295912-299899 (3.9 KB), approx 00:00:00-00:00:02
  recovered ~100.0% of the media (3.9 KB skipped across 1 damaged range(s))
```

A clean source prints `no damage found (equivalent to reindex)` and reports zero damaged ranges.

One honest limit: the recovery is structural, not decode-level. A block whose framing is intact but whose payload tail was overwritten is kept as-is (detecting it would require decoding the codec bitstream, which mkvgo never does) -- expect at worst one glitched frame at a damage boundary.

### rollback

Reconstruct the pre-repair original from a repaired file and the delta written by `--rollback-delta` (available on `reindex`, `reindex-inplace` and `salvage`). The delta replaces a full backup copy: it contains "copy this range of the repaired file" instructions for everything the repair carried over verbatim (~99.99% of a typical file) plus the literal bytes the repair dropped or rewrote, so it is typically well under 0.1% of the source size.

```
mkvgo rollback <repaired.mkv> <delta.rbd> <restored.mkv>
```

The reconstruction is hash-gated twice: it refuses to run when the repaired file changed since the repair was taken (its sha256 no longer matches the delta entry), and it never delivers an output that does not hash back to the original exactly. A torn or corrupted delta entry (per-entry crc32c) is refused the same way. On any refusal no output file is left behind.

```bash
mkvgo reindex damaged.mkv repaired.mkv --resync --rollback-delta damaged.rbd
mkvgo rollback repaired.mkv damaged.rbd original-restored.mkv
```

---

## Remux

### to-mp4

Remux an MKV/WebM file to MP4 without transcoding. Compressed samples are copied verbatim. Supported codecs: H.264/HEVC/AV1/VP9 video; AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio; SRT and WebVTT subtitles (→ tx3g; WebVTT can also be carried natively, see below). Colour/HDR, chapters and B-frame ordering are preserved, along with the movie title (`©nam`), the other global tags (ARTIST/ALBUM/GENRE/… → iTunes `ilst` atoms), per-track names (`hdlr`/`udta/name`) and language - the symmetric counterpart of `from-mp4`.

```
mkvgo to-mp4 [--faststart] [--skip-unsupported] [--flatten-subs] [--webvtt-native] [--mp3-container-delay] [--hash] <input.mkv> <output.mp4>
```

- `--faststart` writes the `moov` box before `mdat` (one extra pass), for progressive HTTP playback.
- `--skip-unsupported` drops tracks whose codec MP4 cannot carry (e.g. TrueHD) and reports each, instead of failing the whole remux.
- `--flatten-subs` carries ASS/SSA subtitles (which have no native MP4 form) as plain `tx3g` timed text. Lossy - all styling, positioning and karaoke is discarded.
- `--webvtt-native` carries WebVTT as native `wvtt` (ISO/IEC 14496-30) instead of the default `tx3g`. `wvtt` is lossless and read by Apple/Safari/CMAF, but **not** by every mainstream demuxer; leave it off for the widest compatibility.
- `--hash` computes each track's content SHA-256 while the samples stream (no extra I/O) and stores them as freeform `ilst` atoms - the MP4 becomes self-verifying via `mkvgo verify`.
- `--mp3-container-delay` carries an MP3 track's encoder delay as an edit list (like AAC). **Off by default**, because MP3's delay is already in its in-band Xing/LAME header - a derived edit list over-trims and desyncs a native MKV/WebM MP3. Opt in only to round-trip an MP3 that originated in an MP4 (rare), and pass it to `from-mp4` too.

Subtitles never fail the remux: SRT and WebVTT are carried as `tx3g` by default; a subtitle whose format cannot be carried (e.g. ASS without `--flatten-subs`, or bitmap PGS/VOBSUB) is dropped with a reason.

Cover art IS carried: the first JPEG/PNG image attachment (one named `cover.*` preferred) becomes the iTunes `covr` atom, and `from-mp4` brings it back as an attachment.

Not carried into MP4: **other attachments** (fonts - note an ASS track flattened with `--flatten-subs` loses its attached fonts), **track-targeted tags**, and global tags outside the mapped set (ARTIST, ALBUM, DATE_RELEASED, GENRE, COMMENT, ENCODER, COMPOSER, DESCRIPTION). Nested and untitled chapters are flattened out, and the Nero `chpl` chapter list caps at 255 entries (the QuickTime chapter track carries the full list).

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

> The streaming commands (`to-hls`, `hls-segment`, `to-abr`, `concat-hls`) are
> covered end to end - modes, sources, ABR, concat, security, trick-play - in
> the **[streaming guide](streaming.md)**. Below is the per-flag reference.

### to-hls

Package a media file - **MKV/WebM or MP4/MOV** (sniffed from the first bytes)
 - as a fragmented-MP4 / **CMAF** presentation in an output directory, served through two manifests over the same segments: **HLS** (`master.m3u8` with `BANDWIDTH`/`RESOLUTION`/`CODECS`, plus one media playlist per rendition) and **DASH** (`manifest.mpd`, one AdaptationSet per rendition). Tracks are demuxed - the video rendition (`playlist.m3u8`, `init.mp4`, `seg00001.m4s` …) and one rendition per audio track (`audio1.m3u8`, `init_a1.mp4`, `seg_a1_00001.m4s` …), declared as an `EXT-X-MEDIA` AUDIO group - so multi-audio sources (VF/VO) get native language selection in hls.js/Safari/dash.js. No transcoding - samples are copied verbatim into CMAF fragments, so only the codecs `to-mp4` supports are carried (H.264/HEVC/AV1/VP9 video, AAC/Opus/AC-3/E-AC-3/FLAC/MP3/DTS audio). Segments are cut on video keyframes and are independently decodable, so a player can start at any segment. Secondary video tracks are dropped with a reason.

Text subtitle tracks (SRT, WebVTT, ASS/SSA flattened to plain text) ride as segmented **WebVTT renditions** (`subN.m3u8` + `subN_*.vtt`), declared in the master playlist with their language/name/default/forced flags; bitmap subtitles (PGS/VOBSUB) are dropped with a reason. This is the CMAF "copy rung" of an HLS ladder - the packaging, not the encoding: bitrate variants (real adaptive streaming) still require a transcoder.

```
mkvgo to-hls <input.mkv> -o <dir> [-segment 6] [--sub-offset <ms>] [--audio-shift <track>=<ms>] [--chapter-markers]
```

| Flag | Description |
|---|---|
| `-o` | Output directory (required) |
| `-segment` | Target segment length in seconds (default 6). Segments are cut on the first video keyframe at/after each multiple |
| `--keep-tracks` | Comma-separated Matroska track IDs to carry (a **Virtual Edit Layer**): serve a "VF only", "VO + English subs" or "clean" version from one source, no copy - just a different track subset. Video is required |
| `--sub-offset` | Shift every WebVTT subtitle cue by this many milliseconds (negative allowed) -- a virtual resync, no file rewritten. A cue whose shifted end is at or before 0 is dropped; one straddling 0 is clamped to start at 0 |
| `--audio-shift` | Re-base an audio track in presentation (`track=ms`, repeatable; positive = the track's content starts late and is presented earlier - feed it `diagnose`'s per-track delay). The samples are copied verbatim and the media segments are byte-identical with or without the shift: only the init segment's edit list moves, so a constant A/V desync is cancelled in the served stream without touching the source. Over-shifts clamp to the presentation start. The persistent repair remains `mkvgo retime` |
| `--chapter-markers` | Opt-in: expose the source's chapters as `EXT-X-DATERANGE` lines in the video media playlist and a chapter `EventStream` in `manifest.mpd` - chapter navigation and ad-insertion cue points, no re-segmentation. See [streaming.md](streaming.md#chapter-markers-and-ad-insertion-points) |

```bash
mkvgo to-hls video.mkv -o stream/
mkvgo to-hls video.mkv -o stream/ -segment 4
mkvgo to-hls multi.mkv -o vf/    --keep-tracks 1,2      # "VF only": video + French audio, no other tracks
mkvgo hls-segment multi.mkv master --keep-tracks 1,2,5  # same subset, on demand
mkvgo to-hls video.mkv -o stream/ --sub-offset -350     # subtitles resynced 350ms earlier
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
the existing segments - zero extra media, what players use for scrubbing
previews. Not emitted when encrypting. Both MKV/WebM and MP4/MOV sources get
it, full pass and on-demand plan alike -- see [streaming.md](streaming.md#trick-play-scrubbing)
for the on-demand cost model (MP4 is free at plan time; Matroska builds it
lazily, on the first `iframe.m3u8` request, from block headers only).

`--single-file` packs each rendition into ONE progressive file (`stream.mp4`,
`stream_a1.mp4` …: init + `sidx` + all fragments) served by byte ranges - the
HLS playlists use `EXT-X-BYTERANGE` and the DASH manifest the on-demand
profile's `SegmentBase`/`indexRange`. Two media files instead of hundreds:
friendlier to object storage; the server only needs `Range` support.
Incompatible with `--aes-key` and `--cenc-*`.

Security flags (shared with `hls-segment`; both must use the same values):

- `--aes-key <32 hex>` + `--aes-key-uri <uri>` - encrypt every media segment
  with AES-128-CBC (whole-segment, PKCS#7, IV = segment sequence, per RFC
  8216) and write the `EXT-X-KEY` line. The key itself is never written to the
  output - serving it (with authentication) is the server's job. Init segments
  and subtitles stay clear. AES-128 is HLS-only: no `manifest.mpd` is emitted
  for an encrypted presentation. Supported by hls.js; Safari/FairPlay requires
  SAMPLE-AES (see `--cenc-*` below). some demuxers do not decrypt
  whole-segment fMP4 - verified spec-conformant by openssl round-trip.
  Incompatible with `--cenc-*` (pick one scheme).
- `--aes-rotate-segments <N>` - rotate the AES-128 key every N segments for
  forward secrecy (a captured key decrypts only its own period). Pass
  comma-separated lists to `--aes-key` and `--aes-key-uri` (at least two,
  matching in count); the keys cycle every N segments, key *i* served at URI
  *i*, and the media playlist carries a fresh `EXT-X-KEY` at each boundary. The
  schedule is a pure function of the segment index, so `hls-segment` serves the
  same bytes on demand. Example: `--aes-rotate-segments 10 --aes-key
  <hexA>,<hexB> --aes-key-uri https://k/a,https://k/b`.
- `--cenc-scheme cenc|cbcs --cenc-key <32 hex> --cenc-kid <32 hex> --cenc-iv <16|32 hex> [--cenc-key-uri <uri>]` --
  sample-level Common Encryption (ISO/IEC 23001-7): `cenc` is AES-CTR (a
  per-sample IV, `--cenc-iv` 8 or 16 bytes hex); `cbcs` is AES-CBC with a
  1-encrypted:9-clear 16-byte-block pattern on video (`--cenc-iv` must be 16
  bytes, used as a constant IV). Unlike `--aes-key`, a CENC presentation still
  gets a `manifest.mpd` (`ContentProtection` with the key ID). Media playlists
  (video and audio) carry `EXT-X-KEY` (`METHOD=SAMPLE-AES-CTR` for cenc,
  `METHOD=SAMPLE-AES` for cbcs, `KEYFORMAT="identity"`); no I-frame playlist is
  emitted (a ciphertext byte range is not independently decryptable). Packaging
  only -- no license server: `--cenc-key-uri` is what an EME-capable player's
  DRM path resolves through its own CDM; left unset it falls back to a `data:`
  URI embedding the key (fine for local testing, never for production). Video
  may be H.264, HEVC, AV1 or VP9 - each codec's decoder-visible header bytes
  stay clear and the coded data is protected (verified decrypting and decoding
  in a real Clear Key player); AV1/VP9 refuse rather than mis-protect the frame
  constructs their parsers do not yet cover. See
  [streaming.md](streaming.md#securing-delivery) for the library form
  (`mp4.CENCOptions`) and the exact clear/protected byte rules.
- `--url-prefix <prefix>` - prepend a base to every URI the playlists and the
  MPD reference (CDN base, or a token route). The library form is
  `Options.RewriteURL func(name) string`, which can append per-resource signed
  tokens instead.

### hls-segment

Serve one resource of an HLS presentation **on demand** - nothing is
pre-generated. For a Matroska source, `PlanHLS` reads the metadata, Cues and
first/last clusters (a few bounded reads, ranged when the source is a URL);
for an **MP4/MOV source the moov sample table IS the index**, so the plan is
exact by construction - every resource including the master playlist, the
DASH manifest and the I-frame playlist is byte-identical to the full pass.
It then builds
just the requested resource: the master or media playlist, the init segment,
or the N-th media segment (1-based, matching the playlist's `segNNNNN.m4s`).
A media segment is built by seeking straight to its window through the Cues
and reading only that window - first-play latency is milliseconds and storage
cost is zero, whatever the file size.

```
mkvgo hls-segment <input.mkv|url> <master|playlist|init|N> [-o out] [-segment 6] [--sub-offset <ms>] [--audio-shift <track>=<ms>] [--synthesize-index] [--chapter-markers]
```

Without `-o` the resource goes to stdout (pipe it from a server handler).
`-segment` must match across calls -- it defines the boundaries. `--sub-offset`
(see `to-hls`) resyncs the WebVTT renditions virtually; it must match `to-hls`'s
value for the two modes to stay byte-identical. `--audio-shift` (see `to-hls`)
cancels a constant A/V desync in the served segments via the init's edit list,
nothing rewritten.

The output is **byte-identical** to the corresponding file `to-hls` writes
(same boundaries, same fragments, cover art and global tags in the init), so
pre-generated and on-demand serving can be mixed transparently. Text subtitle
tracks are declared in the master playlist and served as segmented WebVTT
renditions (`subN.m3u8` + windowed `subN_00001.vtt`…, plus the whole track as
`subN.vtt`); text blocks have no cue index, so the cues come from scanning
the clusters - incremental, bounded and cached: a windowed request costs one
bounded read whether playback is sequential or seeking, only the whole-track
`subN.vtt` costs a full pass. The remaining difference from `to-hls`: the master
playlist's `BANDWIDTH` is estimated from the source's cluster sizes. The
source must carry a Cues index (`mkvgo reindex` adds one) - or pass
`--synthesize-index`: when the index is missing or references no video
keyframes, the plan walks the clusters once (block headers only, no payload
bytes) and synthesizes the cue points in memory instead of refusing. Nothing
is written - the road to seekable on-demand playback for a source on a
read-only mount; a corrupt cluster stream still refuses (repair first). Each
`hls-segment` invocation re-walks, so this flag suits the long-running `serve`
(one plan, many requests) better than per-segment CLI calls.

```bash
mkvgo hls-segment movie.mkv master                 # multivariant playlist → stdout
mkvgo hls-segment movie.mkv 42 -o seg00042.m4s     # just that segment
mkvgo hls-segment movie.mkv seg00042.m4s           # same, by player-facing name
mkvgo hls-segment movie.mkv sub1.vtt               # a subtitle rendition
mkvgo hls-segment https://nas/movie.mkv 3          # remote: reads only segment 3's ranges
```

### serve

Serve one file's on-demand HLS presentation over plain HTTP -- `hls-segment` wrapped in a long-running `net/http` server (`mkvhttp.Handler`), so a player can be pointed straight at it: nothing is written to disk, every resource is built the first time it is requested.

```
mkvgo serve <file.mkv|url> [-addr :8478] [-segment 6] [--keep-tracks 1,2 | --keep-lang fre] [--window-cache <MiB>|off]
mkvgo serve <file.mkv|url> [-addr :8478] --direct
mkvgo serve <file.mkv|url> [-addr :8478] --auto [-target mse-generic]
```

- `-addr` sets the listen address (default `:8478`); the command prints the playable URL on startup.
- Accepts the same shared HLS flags as `to-hls`/`hls-segment` (`-segment`, `--keep-tracks`, `--keep-lang`, ...) when packaging (the default mode, and the mode `--auto` falls back to).
- `--window-cache <MiB>`: what the plan may hold for a window's un-collected renditions. A viewer asks for the video and the audio of the same instant, and one walk of the source builds both (their bytes are interleaved - reading one rendition's window reads them all), so the second request costs no read at all: a viewer reads the source once instead of twice, measured 2.1-3.7x down to 1.07-1.84x on real releases. Omit the flag and the plan sizes the budget from the source; `off` disables the sharing, and every rendition re-walks its own window.
- Responses carry a strong ETag (SHA-256 of the bytes), support `Range` (206 partial content) and conditional `If-None-Match` (304), and set `Cache-Control` per resource class: `no-cache` for playlists/manifests (`.m3u8`/`.mpd`), `public, max-age=31536000, immutable` for everything else (segments and init segments are a deterministic function of the source, so caching them forever is safe). CORS headers are enabled, for a browser-based player on another origin.
- `--direct`: skip packaging entirely and serve the raw file byte-range (`mkvhttp.FileHandler`) -- direct-play: serve the file as-is when the client supports it, no packaging.
- `--auto`: run `mkvgo playability` (target `-target`, default `mse-generic`) and pick `--direct` when the overall verdict is direct-play, or the on-demand HLS plan otherwise; prints which mode it chose. `--direct` and `--auto` are mutually exclusive.
- Ctrl-C shuts the server down gracefully.

```bash
mkvgo serve movie.mkv -addr :8080
# serving movie.mkv
#   http://localhost:8080/master.m3u8

mkvgo serve movie.mkv -addr :8080 --direct
# serving movie.mkv (direct-play, no packaging)
#   http://localhost:8080/movie.mkv

mkvgo serve movie.mkv -addr :8080 --auto
# auto: direct-play verdict for target "mse-generic" -> direct file (no packaging)
# serving movie.mkv (direct-play, no packaging)
#   http://localhost:8080/movie.mkv
```

See `docs/library.md` (`mkvhttp.Handler`, `mkvhttp.FileHandler`) for the full caching/ETag/Range semantics tables, and `docs/streaming.md` for a short pointer on serving over HTTP from library code.

### serve-growing

Play while downloading: serve a file that may still be growing (a download landing on disk) as HLS -- `mp4.PlanGrowingHLS` wrapped in the same long-running server `serve` uses. The media playlist is `EVENT`-typed and lengthens as new whole clusters land; once the source finishes (a Cues index appears, or the file simply stops growing and `Complete()` is signalled), it switches to `VOD` + `ENDLIST`.

```
mkvgo serve-growing <file.mkv> [-addr :8478] [-segment 6]
```

- `-addr` sets the listen address (default `:8478`); `-segment` sets the target segment duration in seconds (default 6), like `serve`/`to-hls`.
- Polls the source once a second for new whole clusters (a stat plus, at most, a bounded read of what's new) and extends the playlist as they land; a partial trailing cluster (still being written) is never served from.
- A published segment's bytes are byte-identical to what `to-hls`/`hls-segment` would produce for the finished file, and never change once served (stable numbering).
- Encrypted/CENC presentations, subtitle renditions, the DASH manifest and the I-frame trick-play playlist are not available in this version (see `docs/library.md` for the full list of v1 limits).
- Ctrl-C shuts the server down gracefully.

```bash
mkvgo serve-growing downloading.mkv -addr :8080
# serving downloading.mkv (play while downloading)
#   http://localhost:8080/master.m3u8
```

See `docs/streaming.md` ("Play while downloading") for the mechanics (cursor, partial-cluster rule, byte-identity) and `docs/library.md` (`mp4.PlanGrowingHLS`) for the full API.

### to-abr

Package several **pre-encoded quality variants** of the same content into one
multi-variant HLS presentation - "ABR light": mkvgo does the packaging (no
transcoding; producing the encodes remains a transcoder's job). The first
source is the reference: its audio tracks and subtitles serve every variant;
the other sources contribute only their video rendition (packaged
`VideoOnly`). Each source lands in `v1/`, `v2/`, … and the top `master.m3u8`
declares one variant per source with its real `BANDWIDTH`/`RESOLUTION`/
`CODECS` over the shared audio/subtitle groups.

```
mkvgo to-abr -o <dir> <best.mkv> <lower.mkv> [...] [-segment 6] [--sub-offset <ms>] [--chapter-markers] [--aes-key … --aes-key-uri …] [--url-prefix …]
```

For seamless switching the sources should share the keyframe cadence (same
GOP length, keyframes at the same times - encode each variant with a fixed GOP
and forced keyframes). The combined **HLS** master always plays; a mismatched
cadence still switches, just realigning on the next keyframe.

A combined **DASH** `manifest.mpd` (one video AdaptationSet, a Representation
per variant) is written at the top level **only when the variants are
segment-aligned** - DASH shares one SegmentTimeline across a switch set, so it
is unsafe otherwise. When they are not aligned, only each variant's own
`v{k}/manifest.mpd` is written.

```bash
mkvgo to-abr -o stream/ movie-1080p.mkv movie-720p.mkv movie-480p.mkv
```

The variants may be Matroska/WebM **or** progressive/fragmented (CMAF) MP4 - a
pre-encoded ladder that is already fragmented MP4 is read directly.

### abr-segment

Serve one resource of the multi-variant presentation **on demand** - the
on-demand counterpart of `to-abr` (as `hls-segment` is to `to-hls`). Nothing is
pre-generated: each resource is built when requested, and a remote variant
(httpfs) transfers only the ranges a viewer watches. The first argument is the
resource name (`master.m3u8`, or `v{k}/<name>` such as `v2/seg00042.m4s`), the
rest are the quality variants best first.

```
mkvgo abr-segment <master.m3u8|v{k}/name> <best> <lower> [...] [-o out] [-segment 6] [--sub-offset <ms>] [--chapter-markers]
```

```bash
mkvgo abr-segment master.m3u8 movie-1080p.mp4 movie-720p.mp4     # top manifest
mkvgo abr-segment v2/seg00042.m4s 1080p.mp4 720p.mp4 -o s.m4s    # one segment
```

### watermark-segment

Serve one resource of a forensic A/B session-watermarked stream. Two GOP-aligned
encodes of one title (variant A and B, imperceptibly different) are served as one
HLS presentation whose per-segment bytes come from A or B by a per-viewer bit, so
a leaked copy carries a signature identifying the session. The manifest
(`master`/`playlist`/`init`) is shared; a media segment `N` is drawn from A by
default, from B with `--variant B`, or routed by bit `N` of `--pattern` (hex,
LSB-first per byte). No re-encode. The session-to-code assignment (and
collusion-resistant codes) is the caller's policy.

```
mkvgo watermark-segment <a.mkv> <b.mkv> <master|playlist|init|N> [--variant A|B] [--pattern <hex>] [-o out] [-segment 6]
```

```bash
mkvgo watermark-segment a.mkv b.mkv playlist -segment 6           # shared media playlist
mkvgo watermark-segment a.mkv b.mkv 7 --pattern 4b -o s.m4s       # segment 7, routed by bit 7 of 0x4b
```

### forensic-segment

The single-source flavor of `watermark-segment`: variant B is DERIVED from the one source, with no second encode - each variant-B segment has one disposable H.264 frame removed at the sample level (a frame no other frame references, `nal_ref_idc == 0`), timing-compensated so the manifest, `#EXTINF` durations and the decode timeline of every following segment are identical to variant A's. The difference lives in the coded samples, so it survives a remux; it does not survive a re-encode. A viewer of variant B sees at most a one-frame hold (~40 ms).

Not every segment carries a bit: a segment with no disposable frame (all-intra, or every frame referenced) has identical variants - `--distinct` reports whether segment `N` is a carrier. A reliable session signature wants a healthy number of carrier segments (~12+); if most segments report `false`, prefer the two-encode `watermark-segment`.

```
mkvgo forensic-segment <src.mkv|mp4> <master|playlist|init|N> [--variant A|B] [--pattern <hex>] [--distinct] [-o out] [-segment 6]
```

- H.264 video only in this version (HEVC signals disposability differently); encryption is refused - encrypt the served bytes at the edge.
- `--distinct N` prints whether segment N differs across variants instead of emitting bytes.

```bash
mkvgo forensic-segment movie.mkv 7 --distinct                     # does segment 7 carry a bit?
mkvgo forensic-segment movie.mkv 7 --variant B -o segB00007.m4s   # the derived variant
```

The library equivalent is `mp4.PlanForensic` + `mp4.DropNonRefSample` (see library.md).

Every `v{k}/<name>` is byte-identical to the file `to-abr` would have written.
The library equivalent is `mp4.PlanABR` (see library.md).

### concat-hls

Package several sources - e.g. consecutive episodes - as **one continuous HLS
session**: a single `master.m3u8`/`playlist.m3u8`/`audioN.m3u8`[/`subN.m3u8`]
spanning every source, so a player never reloads and never sees a session
boundary. This is not ABR (the sources are not quality variants of the same
content); it concatenates different content in playback order.

```
mkvgo concat-hls <in1> <in2> [...] -o <dir> [-segment 6]
```

```bash
mkvgo concat-hls -o stream/ ep01.mkv ep02.mkv ep03.mkv
```

Each source packages into its own `p0/`, `p1/`, `p2/` … exactly like `to-hls`
would on its own: no re-timestamping, no copy. The concatenated playlists
reference those parts' segments directly, with `EXT-X-DISCONTINUITY` marking
each boundary (`EXT-X-VERSION:6`, the version an `EXT-X-MAP` in a media
playlist needs).

Every source must share the same video codec family and the same kept-audio
layout (count, codec, language, in order); otherwise `concat-hls` refuses up
front, before anything is written, listing every mismatch. Subtitles ride
along only when every source exposes the same rendition layout
(count/language/name/forced); otherwise they are dropped (reported the same
way `--skip-unsupported` drops a track) and the video/audio concatenation
still plays. A surviving subtitle's cue times are shifted onto the
concatenated timeline by the cumulative duration of the sources before it (the
one place concat rewrites content - WebVTT cue times are not reset by
`EXT-X-DISCONTINUITY` the way the CMAF fragments are).

v1 does not support `--aes-key`/`--single-file`, and emits no combined DASH
manifest and no combined I-frame playlist (see streaming.md for why).

### concat-segment

Serve one resource of a concatenated session **on demand** - the on-demand
counterpart of `concat-hls` (as `hls-segment` is to `to-hls`). Nothing is
pre-generated. The first argument is the resource name (`master.m3u8`, or
`p{k}/<name>` such as `p1/seg00042.m4s`, `k` 0-based), the rest are the sources
in playback order.

```
mkvgo concat-segment <master.m3u8|p{k}/name> <in1> <in2> [...] [-o out] [-segment 6]
```

```bash
mkvgo concat-segment master.m3u8 ep01.mkv ep02.mkv ep03.mkv          # top manifest
mkvgo concat-segment p1/seg00042.m4s ep01.mkv ep02.mkv ep03.mkv -o s.m4s  # one segment
```

Every `p{k}/<name>` is byte-identical to the file `concat-hls` would have
written. The library equivalent is `mp4.PlanConcat` (see library.md).

### extract-frame

Extract the video keyframe nearest a timestamp, **decoder-ready** - the mkvgo
half of a thumbnail/storyboard pipeline. The keyframe is seeked through the
Cues (a few bounded reads, no scan) and packed so a decoder ingests it
directly: Annex-B with the SPS/PPS (H.264) or VPS/SPS/PPS (HEVC) prepended, or
a minimal IVF wrapper for VP8/VP9/AV1. mkvgo never decodes - turning the
sample into an image is one decoder call away.

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

## Playability, ABR Ladder and Ingest

### playability

Per-track and overall verdict for whether a file plays on a given target
directly, needs only a container remux, or needs a transcode - from
head-only metadata, no external probe, no decode.

```
mkvgo playability [-target safari|chrome|firefox|chromecast-gen3|mse-generic|chromium-generic|brave|opera|vivaldi|samsung-internet|edge] [-json] <file.mkv|.mp4|url>
```

Default target is `mse-generic` (plain H.264/AAC, capped at High@4.1 - the
safe universal bar for a generic MediaSource Extensions player). `-json`
prints the full `PlayabilityReport` (per-track `Verdict`/`Reasons`, overall
verdict, suggested `RemuxContainer`); without it, one line per track plus the
overall line.

```bash
mkvgo playability movie.mkv
# Target: mse-generic
#   #1  video     remux  (source container "mkv" does not carry this codec on target "mse-generic"; remux to mp4 carries it without a transcode)
#   #2  audio     remux  (source container "mkv" does not carry this codec on target "mse-generic"; remux to mp4 carries it without a transcode)
# Overall: remux
# Suggested remux container: mp4

mkvgo playability -target safari -json movie.mkv | jq .OverallVerdict
```

The library equivalent is `matroska.Playability` / `matroska.TargetByName`
(see library.md for the full capability table and how to override it with a
custom `Target`).

### ladder

Recommend an ABR ladder from the source's own resolution/bitrate/codec:
capped at the source (never upscales, never exceeds the source bitrate),
scaled by a documented codec efficiency factor. Guidance for an external
encoder, not a guarantee - mkvgo never transcodes.

```
mkvgo ladder [-json] <file.mkv|.mp4|url>
```

```bash
mkvgo ladder movie.mkv
#   1080p    1920x1080     6000 kb/s
#   720p     1280x720      3000 kb/s
#   480p      854x480      1200 kb/s
#   360p      640x360       700 kb/s

mkvgo ladder -json movie.mkv | jq -r '.[] | "\(.Label) \(.BitrateKbps)k"'
```

The library equivalent is `matroska.RecommendLadderFor` / `RecommendLadder`
(see library.md).

### ingest

One-call serving decision for a media server's per-file onboarding: composes
`playability`, `ladder` and a reindex into a single plan, so a caller does not
have to chain "check playability, then check the seek index, then maybe
reindex, then maybe recommend a ladder" by hand. No decode, no transcode - a
`transcode` strategy only returns a recommended ladder for an external
encoder to run.

```
mkvgo ingest [-target name] [-reindex] [-analyze] [-json] <file.mkv|.mp4|url>
```

| Flag | Description |
|---|---|
| `-target` | Playback target (same names as `playability`; default `mse-generic`) |
| `-reindex` | Patch in a seek index (in-place) if a remux decision needs one and none exists |
| `-analyze` | Also run `analyze` and attach its report to the plan |

Sample output for each strategy:

```bash
# direct-play: source already plays as-is on the target
mkvgo ingest -target mse-generic already-mp4-h264-aac.mp4
# Target: mse-generic
# Source container: mp4
# Strategy: direct-play
# Reasons:
#   - target "mse-generic", source container "mp4"
#   - every track direct-plays on "mse-generic" from container "mp4"; serving the source as-is

# remux-hls: codec is fine, container needs to change; a seek index is missing
mkvgo ingest -target safari -reindex movie.mkv
# Target: safari
# Source container: mkv
# Strategy: remux-hls
# Remux container: mp4
# Seek index present: true
# Reindexed in place: yes
# Reasons:
#   - target "safari", source container "mkv"
#   - source container "mkv" does not carry every track's codec on "safari"; remux to mp4 keeps every codec, no transcode
#   - source has no head-discoverable seek index; a reindex is required before on-demand HLS packaging
#   - in-place reindex succeeded; seek index is now head-discoverable

# transcode: codec/profile/level unsupported on the target
mkvgo ingest -target chrome hevc-main10.mkv
# Target: chrome
# Source container: mkv
# Strategy: transcode
# Recommended ladder:
#   1080p    1920x1080     3600 kb/s
#   720p     1280x720      1800 kb/s
#   480p      854x480       720 kb/s
#   360p      640x360       420 kb/s
# Reasons:
#   - target "chrome", source container "mkv"
#   - at least one track's codec/profile/level is not carried by any container "chrome" accepts; transcode required, recommended ladder has 4 rung(s)

mkvgo ingest -json movie.mkv    # ServingPlan
```

When `-reindex` is set but the file's layout cannot hold a head-discoverable
seek index in place (see `ErrIndexNotHeadDiscoverable` in library.md), the
plan is still returned - not an error - with `ReindexInPlacePossible: false`
and a `Reasons` entry pointing at a copy reindex (`mkvgo reindex`) instead.

The library equivalent is `matroska.Ingest` (see library.md).
