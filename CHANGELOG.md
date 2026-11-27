# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the latest release and, above it, the changes not released yet when
there are any; an older version moves to its series' file in
[changelog/](changelog/) when a newer one ships.

## [0.73.0] - 2026-11-27

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
  on-demand plan keeps the source's `BANDWIDTH`. See docs/library.md. The
  profile 8.1 output has not been validated on a Dolby Vision device.
- `WithDolbyVisionLayer` / `mp4.Options.DolbyVisionLayer` and `-in-band` say
  whether a profile 7 enhancement layer is minimal (MEL) or full (FEL), in
  `Track.DolbyVision.EnhancementLayer`, from the last 8 KiB of the first
  sample; other files read nothing more. Over HTTP a Matroska probe adds at
  most one request of 8 KiB when the first window holds the block headers.
- `HLSPlan.Codecs` lists each video and audio track of a plan with the
  RFC 6381 string its master writes, and says which have none: the video's
  stays available when an audio track makes the master leave `CODECS` out.
  `mp4.CodecString` gives the same string for one track under the sample
  entry name the caller writes (`hvc1` or `hev1`), for packaging done
  without a plan.
