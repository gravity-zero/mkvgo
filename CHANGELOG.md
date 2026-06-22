# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Dolby Vision configuration.** Video tracks now expose `Track.DolbyVision`
  (profile, level, RPU/EL/BL presence and `bl_signal_compatibility_id`), decoded
  from the `DOVIDecoderConfigurationRecord`:
  - MP4 reads the `dvcC`/`dvvC` box from the video sample entry, and recognises
    the Dolby Vision sample entry types (`dvhe`/`dvh1` over HEVC, `dvav`/`dva1`
    over AVC, `dav1` over AV1) so those tracks are no longer dropped.
  - Matroska/WebM reads the `dvcC`/`dvvC` `BlockAdditionMapping`.
  Exposed via `mkv.DolbyVision` / `mkv.ParseDolbyVisionConfig`, shared by both
  readers, so a probe can report Dolby Vision without a packet scan.

- **Head-only keyframe index on the Container.** `Container.Keyframes` ([]int64,
  milliseconds, ascending, de-duplicated) is now filled by the metadata probe in
  the same pass — no separate call, no second open:
  - MP4 `OpenMeta`/`ReadMeta` derive it from the sync-sample table (`stss`) and
    per-sample timing (`stts`/`ctts`) already parsed from `moov`.
  - Matroska/WebM `OpenMeta`/`ReadMeta` derive it from the `Cues` seek index,
    reached via the `SeekHead` (one seek to one element, no `Cluster` scan); a full
    `Read` exposes it too. nil when the source has no usable index, so a caller can
    fall back to a packet scan.
  This replaces a full packet scan for `-c copy` HLS/DASH segment alignment.

- **MP4 probe reads track-selection metadata.** `OpenMeta` / `ReadMeta` /
  `RemuxFromMP4` now populate, for each track:
  - **language** — `mdhd.language` (ISO 639-2; the QuickTime Macintosh language
    codes are decoded too) and the `elng` box (BCP-47) → `Track.Language` /
    `LanguageBCP47` / `LanguagePresent`.
  - **default flag** — the `tkhd` `track_enabled` flag → `Track.IsDefault` /
    `DefaultPresent`.
  - **forced flag** — the track-level `kind` box with the DASH role scheme
    (`urn:mpeg:dash:role:2011`, value `forced…`) that ffmpeg writes, since MP4 has
    no native forced flag → `Track.IsForced` / `ForcedPresent`.
  - **audio channel count** — read from the codec configuration (AAC
    `AudioSpecificConfig`, AC-3/E-AC-3 `dac3`/`dec3`) instead of the
    `AudioSampleEntry` field, which many muxers leave at 2 for multichannel audio.
  - **colour** — any field the `colr` box omits is filled from the codec
    bitstream (e.g. the H.264 SPS VUI), via the now-exported
    `reader.FillColourFromCodecPrivate`.
- **Dropped (non-carried) tracks are surfaced.** `OpenMeta` / `OpenMetaWithFS` /
  `ReadMeta` return an additional `[]DroppedTrack` listing tracks present in the
  file but not in `Container.Tracks` — cover art / attached pictures and non-media
  tracks (hint, timecode, metadata) — each with its track ID, fourcc and a reason.
  `RemuxFromMP4` reports the same tracks through `Options.OnDrop`.

### Changed

- **BREAKING:** `mp4.OpenMeta`, `OpenMetaWithFS` and `ReadMeta` now return
  `(*mkv.Container, []DroppedTrack, error)` (was `(*mkv.Container, error)`).

## [0.7.2] - 2026-06-22

### Added

- **WebVTT subtitles in MP4 remux.** `RemuxToMP4` now carries WebVTT tracks
  (`S_TEXT/WEBVTT` and the WebM-era `D_WEBVTT/*` ids that ffmpeg writes) instead
  of dropping them:
  - By default a WebVTT track is carried as `tx3g` timed text — the only MP4
    subtitle form read universally (ffmpeg included).
  - **`Options.NativeWebVTT`** (CLI `--webvtt-native`) carries it losslessly as
    native `wvtt` (ISO/IEC 14496-30) instead — cue settings and inline markup are
    preserved and Apple/Safari/CMAF read it, but ffmpeg's MP4 demuxer does not, so
    it is opt-in. `RemuxFromMP4` reads `wvtt` back to `S_TEXT/WEBVTT`.
- **`Options.FlattenStyledSubs`** (CLI `--flatten-subs`) carries ASS/SSA — which
  have no native MP4 form — as `tx3g`, stripping the dialogue framing and override
  tags (`{\...}`, `\N`, `\h`). Lossy: all styling/positioning/karaoke is discarded.
- Subtitles **never fail a remux**: a subtitle whose format cannot be carried is
  now always dropped with a reason via `Options.OnDrop` (pointing at the relevant
  flag), instead of being silently skipped or — for some inputs — aborting.
- New codec short name **`webvtt`** for `S_TEXT/WEBVTT` (reader and writer), for
  parity with `srt`/`ass`/`ssa`.

## [0.7.1] - 2026-06-21

### Added

- **`mp4.OpenMeta`** / **`OpenMetaWithFS`** / **`ReadMeta`** — metadata-only probe
  of an MP4 file, the counterpart of the MKV reader's `OpenMeta`/`ReadMeta`. They
  parse only the movie header (`moov`: track sample entries, colour code points,
  chapters) and return a `*mkv.Container` (Info, Tracks, Chapters, DurationMs)
  **without reading any sample data (`mdat`) or writing an output file**. This is
  the fast path for indexing/scanning an MP4 library — previously the only way to
  read an MP4's codecs/colour/chapters was a full `RemuxFromMP4` to disk.
  `RemuxFromMP4` and the probe now build their Matroska metadata from a single
  shared helper, so they report identical tracks/chapters/duration.

## [0.7.0] - 2026-06-21

New `mp4` package: remux between Matroska/WebM and MP4 (ISO base media file
format) without transcoding. It is isolated from the EBML core (shares no
low-level code with `ebml`/`mkv`) and is experimental.

### Added

- **`mp4.RemuxToMP4`** — MKV/WebM → progressive MP4. Compressed samples are
  copied verbatim into MP4 sample tables.
  - Video: H.264, HEVC, AV1. Audio: AAC, Opus, AC-3, E-AC-3, FLAC, MP3, DTS
    (incl. DTS-HD, carried as `mp4a`/`esds`). Subtitles: SRT (`S_TEXT/UTF8`) →
    `tx3g` timed text.
  - B-frame reordering preserved via a signed `ctts` box; colour/HDR code points
    written as a `colr` (nclx) box; 64-bit `mdat` with `co64` when needed.
  - Chapters written both as a Nero `chpl` box and a QuickTime chapter track
    (`tref`/`chap`).
  - `Options.FastStart` places `moov` before `mdat` for progressive HTTP
    playback; `Options.SkipUnsupported` drops tracks whose codec cannot be
    carried (reported via `Options.OnDrop`) instead of failing.
- **`mp4.RemuxFromMP4`** — MP4 → MKV. Reads the codecs above plus their MP4
  sample entries; colour and chapters round-trip back to the Matroska `Colour`
  element and chapter atoms.
- **`writer.WriteBlockGroup`** — writes a BlockGroup with a BlockDuration; used
  for subtitle cues. `WriteCluster` now emits a BlockGroup for any block with a
  non-zero `Duration`.
- **`Block.Duration`** — new additive field, populated from a BlockGroup's
  BlockDuration by the block reader.
- The MKV writer now emits the `Colour` element for tracks carrying colour code
  points.
- **CLI** — `mkvgo to-mp4 [--faststart] [--skip-unsupported] <in> <out.mp4>`,
  `mkvgo from-mp4 <in.mp4> <out.mkv>`, and `mkvgo to-webm <in> <out.webm>`
  (the latter exposing the existing `RemuxToWebM` on the command line).

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

[0.7.2]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.7.2
[0.7.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.7.1
[0.7.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.7.0
[0.6.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.6.0
[0.5.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.5.0
[0.4.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.4.0
[0.3.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.1
[0.3.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.0
