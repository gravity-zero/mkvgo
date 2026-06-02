# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/).

## [0.4.0] - 2026-06-03

Probe metadata: the track reader now exposes the fields a media indexer needs to
match `ffprobe -show_streams`, and can distinguish "explicitly set in the file"
from "spec default". All struct changes are additive — existing exported fields
and types are unchanged.

### Added

- **Language**
  - `Track.LanguageBCP47` — the IETF BCP-47 language element (`0x22B59D`), now
    parsed alongside the legacy ISO 639-2 `Language` (`0x22B59C`). Modern muxers
    that write only BCP-47 are no longer mis-read.
  - `Track.ResolvedLanguage()` — effective language with BCP-47 taking precedence
    over the legacy element, per the Matroska spec.
- **Presence flags** — tell an explicit value from an applied default:
  `Track.LanguagePresent`, `Track.DefaultPresent`, `Track.ForcedPresent`.
- **Video colour** — parsed from the `Colour` element (`0x55B0`):
  `Track.ColorSpace` (MatrixCoefficients `0x55B1`), `Track.ColorTransfer`
  (`0x55BA`), `Track.ColorPrimaries` (`0x55BB`), `Track.ColorRange` (`0x55B9`) as
  raw CICP / ITU-T H.273 code points, and `Track.VideoBitDepth` (BitsPerChannel
  `0x55B2`). Helpers `ColorSpaceName()` / `ColorTransferName()` /
  `ColorPrimariesName()` / `ColorRangeName()` map them to the exact strings
  ffprobe prints, and `IsHDR()` reports BT.2020 + PQ/HLG signalling.
- **Frame rate** — `Track.FrameRate`, derived from `DefaultDuration` (`0x23E383`,
  `fps = 1e9 / ns`). Video tracks only (ffprobe reports `r_frame_rate` for video).
- **Codec naming** — `FFprobeCodecName()` maps mkvgo's short codec names to
  ffprobe's `codec_name` where they diverge (`srt`→`subrip`,
  `vobsub`→`dvd_subtitle`, `pgs`→`hdmv_pgs_subtitle`, `dvbsub`→`dvb_subtitle`).
  The existing `CodecShortName` values are intentionally kept unchanged.
- The streaming reader (`ReadStream`) parses all of the above at parity with the
  seekable reader.

### Changed

- **Behaviour change — `Language` no longer defaults to `"eng"`.** A track with
  neither a `Language` nor a `LanguageBCP47` element now reports `Language == ""`
  with `LanguagePresent == false` (previously it was synthesized to `"eng"`).
  Consumers that relied on the `"eng"` fallback must handle an empty language
  (e.g. treat empty/`und` as "undefined").
  `IsDefault` still applies the Matroska spec default (`true`) when `FlagDefault`
  is absent, but `DefaultPresent` now reports whether the flag was explicit.

### Notes / known gaps

- Colour fields reflect the **container** `Colour` element only — mkvgo does not
  decode the bitstream. When a muxer omits transfer/primaries/bit-depth from the
  container, those stay `nil`; ffprobe may still report them from the codec VUI.
  This is an explainable difference, verified against ffmpeg 7.1 on a real
  fixture (`mkv/reader/probe_realfile_test.go`, with a live ffprobe equivalence
  test that runs when ffprobe is on `PATH`).
- Audio **channel layout** and **per-track bitrate** are not exposed: Matroska
  stores neither (only channel count). Left unset by design rather than fabricated.

## [0.3.1] - 2026-06-03

### Fixed

- `StreamWriter` rejects block timecodes more than int16 milliseconds (~32 s)
  from the cluster start instead of silently wrapping (`WriteBlock`,
  `WriteBlockInCurrentCluster`).

## [0.3.0] - 2026-06-02

### Added

- WebM output (`ValidateWebM`, `WriteWebM`, `NewWebMStreamWriter`, `RemuxToWebM`).
- Corruption-tolerant reader (resync to the next valid cluster).
- Bounded-memory k-way streaming for Mux/Merge/AddTrack.

### Fixed

- EBML element-ID width validation, cumulative metadata-allocation budget, and
  parser hardening (bounded recursion/allocations); streaming/seekable parser
  parity.

[0.4.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.4.0
[0.3.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.1
[0.3.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.0
