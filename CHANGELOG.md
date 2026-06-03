# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/).

## [0.6.0] - 2026-06-03

`ReadMeta`/`Read` now derive colour/HDR metadata from the codec bitstream when the
container Colour element (0x55B0) is absent. Many files signal colour only in the
codec SPS/VUI; such a track previously read as having no colour. Additive — no API
change, the same `Track` fields are populated more often.

### Added

- **Colour from the codec bitstream**, as a fallback to the container Colour
  element. When the container did not supply a field, it is filled from the
  in-memory `Track.CodecPrivate` (no extra file I/O):
  - **H.264** (avcC → SPS VUI): colour primaries / transfer / matrix, bit depth, profile.
  - **HEVC** (hvcC header + SPS VUI): bit depth and profile from hvcC; primaries /
    transfer / matrix from the SPS VUI.
  - **AV1** (av1C header + sequence-header OBU `color_config`): primaries /
    transfer / matrix, bit depth, profile.
  - **VP9** (vpcC fixed fields, when a CodecPrivate is present) — best-effort.

  The recovered values are CICP / ITU-T H.273 code points feeding the existing
  `ColorSpace`/`ColorTransfer`/`ColorPrimaries`/`ColorRange`/`VideoBitDepth` fields
  and `IsHDR()`. A Colour-less HDR10 track then reports `ColorSpaceName()="bt2020nc"`,
  `ColorTransferName()="smpte2084"`, `VideoBitDepth=10`, `IsHDR()=true` instead of
  empty/SDR.
- New additive field **`Track.Profile`** (e.g. "Main 10"), derived from the SPS.

### Behaviour

- The container Colour element stays **authoritative**: the bitstream only fills
  fields the container left nil (per-field precedence).
- CICP code 2 ("unspecified") from the bitstream is treated as absent (left nil);
  bit depth is constrained to {8, 10, 12}.

### Security

- The SPS / VUI / OBU parsing is **fail-soft**: a truncated, malformed or
  adversarial `CodecPrivate` never errors, panics, hangs or allocates unboundedly
  — the colour fields stay nil and the read continues. The parsers are panic- and
  hang-free on their own — a bounds-checked Exp-Golomb reader with a capped
  leading-zero run, every bitstream-driven loop count and bit width bounded, and
  emulation-prevention stripping; the `recover()` in the dispatcher is only a
  last-resort backstop. **`FuzzCodecColour`** drives random bytes straight at the
  parsers *without* that backstop, so a missing bound surfaces instead of being
  masked — it found and fixed an out-of-range bit depth and an Exp-Golomb-driven
  loop, both kept as regression seeds.

### Codecs covered

H.264, HEVC and AV1 are covered with hermetic byte-fixture tests. VP9 (vpcC) is
best-effort: VP9 colour usually lives in the container or in per-frame headers,
the latter outside the metadata path. VVC / Dolby Vision are out of scope.

## [0.5.0] - 2026-06-03

Fast metadata-only read path for library indexing. Additive — `Read` / `Open`
are unchanged.

### Added

- **`ReadMeta(ctx, r, path)`** plus **`OpenMeta`** / **`OpenMetaWithFS`**,
  mirroring the `Read` / `Open` / `OpenWithFS` trio (also re-exported from the
  `matroska` facade). They return the same `Tracks` + `Info` (and `DurationMs`)
  as a full `Read` — byte-identical, via the same `parseInfo` / `parseTracks`
  logic — but stop as soon as both are parsed:
  - never parse the Cues index, never traverse Clusters;
  - reads are buffered (~2 KiB) so the byte-at-a-time EBML reads cost one syscall
    instead of hundreds (matters on a network-mounted library);
  - a head `SeekHead` is used to jump straight to Info/Tracks, so a file whose
    `Tracks` element sits after the first Cluster still works without scanning.
  - `Chapters`, `Attachments`, `Tags` and `Cues` are left **nil** — call
    `Read` / `Open` for those.
  - Hardened for untrusted input: a forged `SeekHead` cannot make the fast path
    over-read (the `SeekID` size is bounded to a real element-ID width and
    `SeekPosition` offsets are range-checked).

### Performance (measured)

On 5 real 5–9 GB mkvmerge files (`bench/main.go`), per file:

| read                | bytes read | time        |
|---------------------|-----------:|------------:|
| `reader.Read` (full)|   ~180 KB  | ~17,000 ms  |
| `ReadMeta`          |    ~2 KB   |    ~0.2 ms  |
| `ffprobe` (ref)     |   ~1.2 MB  |     ~50 ms  |

`ReadMeta` reads ~90× fewer bytes and is ~80,000× faster than the full `Read`,
and ~600× fewer bytes / ~250× faster than forking `ffprobe`. The full `Read`'s
cost is the Cues index (~790 KB across the five files) plus walking every
Cluster — neither needed for indexing. A media server can now use the in-process
reader for indexing instead of forking `ffprobe` per file.

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

[0.6.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.6.0
[0.5.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.5.0
[0.4.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.4.0
[0.3.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.1
[0.3.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.0
