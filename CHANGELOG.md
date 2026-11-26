# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the latest release and, above it, the changes not released yet when
there are any; an older version moves to its series' file in
[changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Added

- Dolby Vision profile 7 conversion without re-encoding, the base layer byte
  for byte: to profile 8.1 (enhancement layer dropped, each RPU rewritten) or
  to the base layer alone (HDR10). `mp4.Options.DolbyVision` on `RemuxToMP4`,
  `RemuxFromMP4`, `RemuxToHLS`, `RemuxToABR` and the on-demand plans of a
  Matroska source; `matroska.ConvertDolbyVision` and `mkvgo dolby-vision` for
  a file; `--dolby-vision 8.1|base` on `to-mp4`, `from-mp4`, `to-hls` and
  `hls-segment`. Sizes, `dvvC` and `SUPPLEMENTAL-CODECS` follow the
  conversion; a sample that cannot be converted fails with a
  `*mkv.DolbyVisionError`. The `mp4` functions that write or hand out
  samples without converting refuse the option
  (`mp4.ErrDolbyVisionUnsupported`) instead of ignoring it. A converted
  on-demand plan keeps the source's `BANDWIDTH`. See docs/library.md.
- `WithDolbyVisionLayer` / `mp4.Options.DolbyVisionLayer` and `-in-band` say
  whether a profile 7 enhancement layer is minimal (MEL) or full (FEL), in
  `Track.DolbyVision.EnhancementLayer`, from the last 8 KiB of the first
  sample; other files read nothing more. Over HTTP a Matroska probe adds at
  most one request of 8 KiB when the first window holds the block headers.

## [0.72.0] - 2026-11-21

### Added

- `Options.SignedCompositionOffsets` (`--signed-offsets` on `to-hls`,
  `hls-segment`, `serve` and `serve-growing`): a reordered video's composition
  offsets are written signed, with no composition shift and no video edit
  list. A player that ignores the edit list showed the picture late by the
  reordering depth (84 ms on a two-frame pyramid). Off by default.

### Breaking

- The HLS/DASH init segment no longer carries the cover art: a large cover
  made it megabytes a player fetches before its first frame. Title and tags
  stay; `RemuxToMP4` still carries the cover.
- HLS/DASH bytes change for a video the fixes below concern: its init's edit
  list and its segments' composition offsets move together. Drop the cached
  init and segments of one source at once.

### Fixed

- HLS/DASH lost frames of a video that reorders deeper than its opening
  frames show: frames shared a presentation time and players dropped them, a
  quarter to a half of a segment's. The composition shift now covers the
  depth the parameter sets declare, and is measured past a first GOP of a
  frame or two.
- A video that reorders deeper than its parameter sets announce keeps every
  presentation time through a negative composition offset (`trun` version
  1); a reader that pulls its decode clock back for one presents that video
  later by it.
- A frame with a stray timestamp (it presents before more than 16 frames
  decoded ahead of it) presents at its decode time and moves no other
  frame's; full pass and on demand agree on it.
- A segment opens on its keyframe when a frame shares the keyframe's
  timestamp, and served on demand it keeps the frames stored between the two.
