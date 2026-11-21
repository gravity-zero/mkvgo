# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Changed

- The HLS/DASH init segment no longer carries the cover art (`RemuxToHLS`,
  `PlanHLS`, `PlanGrowingHLS`, `to-hls`, `serve`, `hls-segment`): a player
  downloads the init before its first frame, and a large cover made it several
  megabytes. Title and tags stay; `RemuxToMP4` still carries the cover.

### Fixed

- HLS/DASH from an H.264 or HEVC video whose opening frames are in
  presentation order and which reorders frames only later: up to a quarter of
  the frames shared a presentation time with a neighbour, and players dropped
  them. The composition shift now covers the reordering depth the parameter
  sets declare (`max_num_reorder_frames`, `sps_max_num_reorder_pics`).
  **Integration note**: for such a source, and for one that declares a deeper
  reordering than its opening frames show, the init segment's edit list and
  the segments' composition offsets change together - drop cached inits and
  segments of the same source at once.
- A video that reorders deeper than its opening frames and its parameter sets
  announce (parameter sets that change mid-file, or state no depth) keeps
  every frame's presentation time in HLS/DASH: the composition offset of such
  a frame is written negative (`trun` version 1) where it was clamped to zero
  and the frame took its neighbour's time. A reader that pulls its decode
  clock back for a negative offset presents that video later by the deepest
  one (two frames on the files measured).

## [0.71.2] - 2026-11-20

### Fixed

- Under `WithInBandColourFallback` (`-in-band`, `mp4.Options.InBandColour`):
  an HEVC track declared bt2020-10 is read as HLG (`arib-std-b67`) when an
  Alternative Transfer Characteristics SEI of its first sample says so - the
  header alone made an HLG video an SDR one; an H.264 record with no SPS
  (`avc3`) gets its profile, level, pixel format and colour from the SPS its
  samples carry; an MPEG-2 track with no CodecPrivate gets its profile, level
  and pixel format from its first sample. Each such track costs one more
  bounded read under the option.
- A fragmented MP4 read head-only gets the first-sample fallback too
  (`mp4.Options.InBandColour`): its sample tables are empty, so nothing was
  read and an `avc3` or `hev1` track kept no profile, level or colour. The
  first `moof` is read for where the first samples are.
- An MPEG-2 CodecPrivate gives the profile, level and pixel format head-only,
  and a VP8 track its pixel format: they were empty.
- Bitmap subtitles: a DVB block fills and draws a full-size region and one
  picture of it, not four planes (254 bytes of block allocated 208 MiB); a
  block asking for more is passed over as damaged.
- A keyframe walk (`WithKeyframeIndex`, `keyframes -walk`) cancelled on its
  way returns the cancellation: it returned the keyframes met so far with no
  error.
- `concat-hls`, `concat-segment` and `PlanConcat` apply `--sub-offset`
  (`Options.SubtitleOffsetMs`): the option was accepted and ignored.
