# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Breaking

- `demux` of a Matroska/WebM writes five more codecs in the file their tools
  read, where it wrote the bare payloads under the codec's name: AV1 as a
  low-overhead OBU stream with a temporal delimiter a temporal unit (`.obu`;
  was `.av1`, which a decoder read as no frame at all), VP8 and VP9 in IVF
  (`.ivf`; were `.vp8`, `.vp9`), MPEG-4 Part 2 behind its VOS/VOL headers
  (`.m4v`; was `.mpeg4`), ALAC in CAF with its magic cookie and packet table
  (`.caf`; was `.alac`), PCM in WAV (`.wav`; was `.pcm`, or the sample format
  such as `.pcm_f64le`; big-endian samples turned little-endian). `--raw`
  (`DemuxOptions.Raw`) still writes the former files, names included.
- `mp4.DroppedTrack` is now an alias of `mkv.DroppedTrack`, the one struct both
  directions report through (gorelease lists the type and every signature
  carrying it as changed; source that names `mp4.DroppedTrack` compiles as it
  is, the two being one type).

### Changed

- The `dropped track N (codec): reason` lines of `to-mp4`, `from-mp4`,
  `to-hls` (and `to-abr`, `concat-hls`), `split`, `join`, `mux`, `merge` and
  `merge-subtitle` go to stderr, no longer stdout, like those of `serve`,
  `demux` and the segment commands: stdout keeps the command's result alone.
  `--strict` is a global flag, taken by every one of these commands.
- Matroska `edit-inplace` / `EditInPlace`: a head with no room left moves the
  Tags, then the Chapters, the Info and the Tracks to the end of the Segment
  (RFC 9559 6.8) instead of refusing; it still refuses a file with no SeekHead
  or with bytes after its Segment.

### Added

- `demux` writes every stream in the file its codec's tools read, from an MP4
  as from a Matroska (the five codecs an existing Matroska demux changes for
  are under Breaking); written bare, none of them decoded.
  An MP4 track the remux cannot carry follows the remux's rule: the demux
  fails by its name (exit 1) unless `--skip-unsupported`
  (`DemuxOptions.SkipUnsupported`) drops it or `-t` names other tracks; a
  zero-length NAL unit no longer refuses an H.264/HEVC frame.
- `from-mp4` and `demux`: every track left out, a subtitle Matroska has no
  form for (TTML, CEA-608) or a non-media track included, is printed on
  stderr as `dropped track N (codec): reason` (N the file's own track ID),
  exit 0 as before; `--strict` (`Options.Strict`, `DemuxOptions.Strict`)
  fails them by name on any such track. The library reports each one through
  `Options.OnDrop` / `DemuxOptions.OnDrop` as a `mkv.DroppedTrack`
  (`mp4.DroppedTrack` is now that type). `tracks` and `probe` say per track
  whether `from-mp4` carries it: `to_matroska` and `to_matroska_refusal` in
  `-json` (`Track.Unsupported`), `[not carried to Matroska: reason]` in text.
- The same towards MP4 and HLS/CMAF: `to-mp4`, `to-hls` (and the ABR
  commands) and `serve` print every Matroska track they leave out - a
  subtitle the output has no form for, a secondary video track, an encrypted
  or unsupported codec - as `dropped track N (codec): reason` on stderr, exit
  0 as before, and take `--strict` (`Options.Strict`) to fail by name on any
  of them; so do `split`, `join`, `mux`, `merge` and the concatenations for
  the non-media tracks and subtitles they leave out, so the flag means the
  same everywhere. `tracks` and `probe` say per track, on a Matroska, whether each
  target carries it: `to_mp4` / `to_mp4_refusal` and `to_hls` /
  `to_hls_refusal` in `-json` (`mp4.TracksCarriage`), the reasons naming the
  flags. Fixed on the way: HLS reported an ASS track dropped with the MP4
  reason while carrying it as a WebVTT rendition.
- `from-mp4` carries MPEG-4 Part 2 (`mp4v` → `V_MPEG4/ISO/ASP`), TrueHD (`mlpa`
  → `A_TRUEHD`) and ALAC (`alac` → `A_ALAC`), as the mainstream muxer does;
  decoded, the Matroska matches the MP4 (checked with a decoder on real files).
  AC-4 still has no Matroska form.
- `demux` and `matroska.Demux` take an MP4/MOV: its tracks and blocks are those
  `from-mp4` would write, through the same formats (ADTS, Annex-B, Ogg), byte
  for byte what `ffmpeg -c copy` extracts; the tracks are named and selected
  (`-t`) by the MP4's own track IDs, the ones `tracks` lists; `mp4.WalkBlocks`
  hands a reader the remux's tracks before its blocks.
- `keyframes -walk` / `matroska.WithKeyframeWalk`: every keyframe from a pass
  over the Segment, for Cues that index only some of them. Listed from the
  Cues, the text form now says so on stderr.

### Fixed

- `demux` of Vorbis to Ogg: each page's granule counts the samples the decoder
  yields (block sizes and modes from the headers), less the last block's
  DiscardPadding, instead of a guess from the timestamps that cut the end
  short or ran past it.
- `diagnose` and `track-ends`: the bytes a walk skips past an unreadable element
  are no longer called damage (they include the sound media up to the next
  Cluster it could read); the message points to `salvage --dry-run` for the map.
- CLI: `to-mp4`, `from-mp4` and `to-hls` name `--skip-unsupported` where the
  refusal named `Options.SkipUnsupported`; `edit-inplace` refused for want of
  room names the `mkvgo edit` command that rewrites the file.
- HLS from a local MP4 (`serve`, `Open`): an audio segment is read ahead over
  its whole span like a video one; read run by run it broke the video's large
  disk reads (a full read of a 20-minute file: 11,217 reads of 79 KiB, 863 of
  1 MiB with the next entry).
- Linux read-ahead requests (`posix_fadvise` WILLNEED) go in pieces of the
  larger of the disk's `read_ahead_kb` and `max_sectors_kb`, the most the kernel
  reads per call: at the default 128 KiB read-ahead, 7,062 reads of 126 KiB for
  that full read became 863.
- HLS from a local Matroska (`serve`, `Open`): the scan a subtitle rendition
  makes of the clusters ahead of its window asks the disk for its range
  first, as a window's walk does, and from the Cues (or a subtitle index)
  reads no further ahead than the request needs; it read the blocks a few
  KiB at a time from scattered positions, and the pages it left split the
  next windows' read-ahead (a 2 GB MKV with three subtitle tracks, full read
  of every rendition on a fresh plan, HDD: 10,948 disk reads of 184 KiB at
  119 MB/s became 2,900 of 690 KiB at 176 MB/s, a sequential read of the file
  taking 2,300 of 875 KiB at 267; through a FUSE mount 17,400 of 115 KiB at
  107 MB/s became 8,700 of 230 KiB at 141 MB/s; random access unchanged on
  both).
- HLS segments read from a local file: the read-ahead covers the whole window
  across renditions, not the requested segment's own span. A player fetches a
  window's audio and video together; asked from its own first sample, the first
  request left the window to come off the disk in two runs (a fragmented MP4
  with its audio behind each fragment's video: 1,656 disk reads where a
  sequential read takes 1,104).

## [0.65.1] - 2026-11-07

### Fixed

- Matroska: the H.264 in-band scan walks the whole block again (at most 1,024
  NALs, 16 KiB of headers parsed), so a block holding both fields of a frame,
  its first one past 16 KiB, keeps its `field_order` (0.65.0 lost it).

## Older versions

Archived in [changelog/](changelog/): [0.60-0.69](changelog/0.60-0.69.md), [0.56-0.59](changelog/0.56-0.59.md), [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
