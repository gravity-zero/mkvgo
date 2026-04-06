# CLI Reference

```
mkvgo <command> [options]
```

Global flags:
- `-json` -- structured JSON output (supported by inspection commands)
- `--version` -- print version and exit
- `-h`, `--help` -- show help for a command

---

## Inspection

### info

Show container info (title, duration, muxing/writing app).

```
mkvgo info [-json] <file.mkv>
```

```bash
mkvgo info movie.mkv
mkvgo info -json movie.mkv
```

### tracks

List all tracks with codec, language, resolution/channels.

```
mkvgo tracks [-json] <file.mkv>
```

```bash
mkvgo tracks movie.mkv
```

### chapters

List chapters with start/end timestamps.

```
mkvgo chapters [-json] <file.mkv>
```

```bash
mkvgo chapters movie.mkv
```

### attachments

List attachments (fonts, cover art, etc.) with MIME types and sizes.

```
mkvgo attachments [-json] <file.mkv>
```

```bash
mkvgo attachments movie.mkv
```

### tags

Show all tags (target type, track associations, key-value pairs).

```
mkvgo tags [-json] <file.mkv>
```

```bash
mkvgo tags movie.mkv
```

### probe

Full dump of all metadata: info, tracks, chapters, attachments, tags.

```
mkvgo probe [-json] <file.mkv>
```

```bash
mkvgo probe -json movie.mkv | jq '.tracks[] | select(.type == "audio")'
```

### validate

Check MKV structure for issues. Reports errors and warnings.

```
mkvgo validate [-json] <file.mkv>
```

```bash
mkvgo validate movie.mkv
```

### compare

Diff metadata of two MKV files. Shows added, removed, and changed elements.

```
mkvgo compare [-json] <a.mkv> <b.mkv>
```

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
mkvgo demux movie.mkv -o ./streams/
mkvgo demux movie.mkv -o ./streams/ -t 1,2
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
mkvgo extract-attachment movie.mkv 1 -o cover.jpg
```

### extract-subtitle

Extract a subtitle track as SRT or ASS.

```
mkvgo extract-subtitle <file.mkv> -t <trackID> -o <out> [-format srt|ass]
```

| Flag | Description |
|---|---|
| `-t` | Track ID to extract (required) |
| `-o` | Output file path (required) |
| `-format` | Output format: `srt` (default) or `ass` |

```bash
mkvgo extract-subtitle movie.mkv -t 3 -o subs.srt
mkvgo extract-subtitle movie.mkv -t 3 -o subs.ass -format ass
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
mkvgo edit movie.mkv -o out.mkv '{"title":"New Title"}}'
cat patch.json | mkvgo edit movie.mkv -o out.mkv -
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
mkvgo edit-title movie.mkv -o out.mkv "My Movie (2024)"
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
mkvgo edit-track movie.mkv -o out.mkv -t 2 -lang jpn -name "Japanese" -default
```

### edit-inplace

Edit metadata without rewriting the entire file. Only modifies headers -- instant even on large files.

```
mkvgo edit-inplace <file.mkv> '<json>'
```

```bash
mkvgo edit-inplace movie.mkv '{"title":"Quick Fix"}}'
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
mkvgo remove-track movie.mkv -o clean.mkv -t 3,4
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
mkvgo add-track movie.mkv -o out.mkv commentary.mkv:1 -lang eng -name "Commentary"
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
| `-format` | Subtitle format: `srt` (default) or `ass` |
| `-lang` | Language code (e.g. `eng`) |
| `-name` | Track name (e.g. `"English"`) |

```bash
mkvgo merge-subtitle movie.mkv -o out.mkv subs.srt -lang eng -name "English"
mkvgo merge-subtitle movie.mkv -o out.mkv subs.ass -format ass -lang jpn
```

### join

Concatenate multiple MKV files sequentially (same codec/track layout required).

```
mkvgo join -o <out.mkv> <file1.mkv> <file2.mkv> ...
```

| Flag | Description |
|---|---|
| `-o` | Output file path (required) |

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
| `-range` | Comma-separated time ranges in milliseconds (0 = end of file) |

```bash
# Split by chapters
mkvgo split movie.mkv -o chapters/ -chapters

# Split first 5 minutes into its own file
mkvgo split movie.mkv -o parts/ -range 0-300000,300000-0
```
