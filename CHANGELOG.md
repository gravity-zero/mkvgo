# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Fixed

- Under `WithInBandColourFallback` (`-in-band`, `mp4.Options.InBandColour`):
  an HEVC track declared bt2020-10 is read as HLG (`arib-std-b67`) when an
  Alternative Transfer Characteristics SEI of its first sample says so - the
  header alone made an HLG video an SDR one; an H.264 record with no SPS
  (`avc3`) gets its profile, level, pixel format and colour from the SPS its
  samples carry; an MPEG-2 track with no CodecPrivate gets its profile, level
  and pixel format from its first sample. Each such track costs one more
  bounded read under the option.
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

## [0.71.1] - 2026-11-20

### Fixed

- Under `WithInBandColourFallback` (`-in-band`, `mp4.Options.InBandColour`),
  three video tracks whose header names no colour now get it from their first
  sample, where they kept none or the wrong "determined": a VP9 track whose
  CodecPrivate holds the Matroska feature records, or whose Colour element
  states the range alone (the keyframe header was read only when the profile
  was missing); an AV1 track whose `av1C` holds no sequence header (a PQ
  video read as having no colour); an HEVC track whose `hvcC` holds arrays
  but no SPS. A VP9 Matroska file with no colour anywhere costs one more
  bounded read under the option.
- A VP9 keyframe header that says its colour is unknown sets
  `ColourDetermined`, as an H.264, HEVC or AV1 stream stating "unspecified"
  does: the colour was read, there is none to find elsewhere.
