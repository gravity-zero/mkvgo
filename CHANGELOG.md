# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/).

## [0.9.0] - 2026-06-23

Field-equivalence pass: the metadata probe reports more of what ffprobe does,
validated against ffprobe over a sweep of real files.

### Added

- **Audio output sample rate for SBR (HE-AAC).** `Track.OutputSampleRate` carries
  the decoder's doubled rate (Matroska `OutputSamplingFrequency` 0x78B5, or the
  AAC `AudioSpecificConfig` SBR extension rate — explicit AOT 5 and the
  backward-compatible 0x2b7 sync extension), and `Track.EffectiveSampleRate()`
  returns what ffprobe reports as `sample_rate`. Read on both the MP4 and Matroska
  paths; the writer persists 0x78B5 for round-trip.
- **Codec level.** `Track.Level` exposes the SPS `level_idc` ffprobe reports as
  `level` (H.264, HEVC, AV1), via the shared codec-bitstream fallback.
- **Display aspect ratio for anamorphic video.** `Track.DisplayWidth` /
  `DisplayHeight` (their ratio is the display aspect), with `DisplayAspectRatio()`
  / `SampleAspectRatio()` helpers returning ffprobe's `display_aspect_ratio` /
  `sample_aspect_ratio`. Read, in precedence order, from the MP4 `pasp` box, the
  Matroska `DisplayWidth`/`DisplayHeight` (0x54B0/0x54BA) elements, or the H.264/
  HEVC SPS VUI `aspect_ratio_info` (Table E-1 `aspect_ratio_idc`, or Extended_SAR
  `sar_width:sar_height`) — the most common H.264 SAR carrier, read head-only from
  the avcC. The ratio is stored exactly (no rounding that would collapse a fine
  pixel aspect), and the helpers reduce it with the same bounded `av_reduce`
  (1024×1024) ffmpeg uses, so the `sar`/`dar` strings match ffprobe on every
  ordinary ratio and tame absurd ones. Both reader paths and the writer handle the
  elements; the MP4 remux emits a `pasp` box so anamorphic display survives a
  round trip.
- **Per-stream average bitrate.** `Track.Bitrate` from the MP4 `btrt` box (or the
  esds `avgBitrate` for AAC).
- **Pixel format.** `Track.PixelFormat` (ffprobe `pix_fmt`, e.g. `yuv420p`,
  `yuv420p10le`) composed from the codec's chroma subsampling and bit depth
  (H.264/HEVC SPS, AV1 colour config, VP9 `vpcC`). For HEVC `hev1` with in-band
  parameter sets, the 4:2:0 chroma of Main/Main 10 is taken from the `hvcC` profile,
  so it still reads head-only.
- **Field order.** `Track.FieldOrder` ("progressive"/"interlaced", ffprobe
  `field_order`) from the Matroska `FlagInterlaced` (0x9A) element or the H.264
  `frame_mbs_only_flag`.
- **MP4 frame rate is read head-only.** `Track.FrameRate` is now derived from the
  `stts` header (media timescale ÷ first `sample_delta`) — ffprobe's `r_frame_rate`
  for constant-frame-rate video — so the metadata probe reports it without
  expanding the sample table (it previously needed `Options{Keyframes: true}`).
- **Display rotation.** `Track.Rotation` (0/90/180/270, clockwise) read from the
  MP4 `tkhd` display matrix — the same matrix ffprobe exposes as Display Matrix
  side data. Lets a player show portrait phone video the right way up.
- CLI `probe` surfaces all of the above (output sample rate, codec profile/level,
  sample/display aspect ratio, bitrate); `-json` carries the new fields.

### Fixed

- **HE-AACv2 Parametric Stereo channel count.** A PS stream codes a mono core
  (`channelConfiguration` 1) the decoder upmixes to stereo; `mp4.OpenMeta` now
  reports 2 channels like ffprobe, detecting both explicit (AOT 29) and
  backward-compatible (0x2b7 / 0x548) signalling.
- **QuickTime `nclc` colr type.** The MP4 probe read only the `nclx` colr type and
  silently skipped the QuickTime `nclc` form, leaving `color_space` empty on files
  that carry their matrix only there. Both types are now read, each CICP field
  taken independently (a matrix-only stream still reports its `color_space`).

### Notes

- Documented three accepted head-only limitations — data present only in the media
  frames, not in any header the probe parses, so ffprobe surfaces it by decoding a
  frame while mkvgo reports the header values: implicit in-band SBR and Parametric
  Stereo (audio), and colour signalled only in an in-band SPS (video).
- On pathological **near-square** sample aspect ratios, ffprobe's `sample_aspect_ratio`
  can differ from mkvgo's: ffprobe re-derives the SAR from the dimension-reduced DAR
  (so the same VUI SAR prints differently at different resolutions), whereas mkvgo
  reports the exact VUI/`pasp` ratio. Display-only and imperceptible (all ≈ the same
  picture shape); mkvgo keeps the true signal rather than mirror that quirk.

## [0.8.0] - 2026-06-23

### Added

- **MP4 metadata probe.** `mp4.OpenMeta` / `OpenMetaWithFS` / `ReadMeta` read an
  MP4's stream metadata head-only, reporting per track: **language** (`mdhd`
  ISO 639-2 — including QuickTime Macintosh language codes — and the `elng` BCP-47
  box), the **default** flag (`tkhd` track_enabled), the **forced** flag (DASH-role
  `kind` box), the **audio channel count** (from the AAC `AudioSpecificConfig` and
  AC-3/E-AC-3 `dac3`/`dec3`, not the unreliable `AudioSampleEntry` field), and
  **colour** code points (`colr`, falling back to the codec bitstream via the now
  exported `reader.FillColourFromCodecPrivate`).
- **Dropped (non-carried) tracks** are surfaced: the probe returns an additional
  `[]DroppedTrack` (cover art / attached pictures, hint/timecode/metadata tracks),
  each with its track ID, fourcc and reason. `RemuxFromMP4` reports them through
  `Options.OnDrop`. (A QuickTime chapter track is not reported — see Fixed.)
- **Dolby Vision.** `Track.DolbyVision` exposes the decoded
  `DOVIDecoderConfigurationRecord` (profile, level, RPU/EL/BL, `bl_signal_compatibility_id`),
  read from the MP4 `dvcC`/`dvvC` box (and the `dvhe`/`dvh1`/`dvav`/`dva1`/`dav1`
  sample entry types) or the Matroska `dvcC`/`dvvC` `BlockAdditionMapping`. It is
  carried through both remux directions, choosing the Dolby sample entry type
  (`dvh1`/`dva1`/`dav1`) for a non-cross-compatible stream and the plain
  `hvc1`/`avc1`/`av01` tag for a cross-compatible one. New `mkv.DolbyVision`,
  `ParseDolbyVisionConfig`, `EncodeDolbyVisionConfig`, `DolbyVision.BoxType`.
- **Head-only keyframe index** on `Container.Keyframes` ([]int64 ms, ascending,
  de-duplicated) — the cut points for `-c copy` HLS/DASH segmentation. MKV/WebM
  fills it from the `Cues` index via the `SeekHead` (one seek, no `Cluster` scan)
  in the metadata pass; MP4 fills it from the `stss`/`stts`/`ctts` tables, with the
  edit list (`elst`) applied as ffmpeg does, opt-in via `Options{Keyframes: true}`.
- **Subtitle extraction to WebVTT**, replacing an `ffmpeg -f webvtt` fork:
  `ops.ExtractSubtitleWebVTT` (embedded MKV/WebM track), `mp4.ExtractSubtitleWebVTT`
  (embedded MP4 tx3g/wvtt track) and `subtitle.FileToWebVTT` (external
  `.srt`/`.ass`/`.ssa`/`.vtt` sidecar), streaming to any `io.Writer`. Building
  blocks: `subtitle.Cue`, `WriteWebVTT`, `FormatVTTTime`, `SRTToCues`, `ASSToCues`,
  `FlattenASSBlock`, `ResolveCueEnds`.
- **Remux preserves what the probe reads.** `RemuxToMP4` writes the default flag
  (`tkhd` track_enabled), the forced flag (`kind` box) and the BCP-47 language
  (`elng`); the MKV writer persists `LanguageBCP47`. The edit list is folded into
  the composition times so `RemuxFromMP4` and the keyframe index present ffmpeg's
  timeline. MP4 video tracks report an average **frame rate** from the sample timing.
- **CLI parity.** `info`/`tracks`/`chapters`/`probe` accept an MP4/MOV path;
  `probe` prints colour, Dolby Vision, the keyframe index and dropped tracks; new
  **`keyframes`** command; `extract-subtitle` gains `-format vtt` (and an MP4
  source); new **`to-vtt`** command for external sidecars.

### Changed

- **MP4 `OpenMeta`/`ReadMeta` are head-only by default**: they read only the `moov`
  box headers and no longer expand the per-sample tables, so probing a long movie
  is far faster (the duration comes from `mvhd`). Pass `Options{Keyframes: true}` to
  build the sample table and populate `Container.Keyframes`. Both gain a variadic
  `...Options` parameter (existing two-arg calls are unaffected).
- **BREAKING:** `mp4.OpenMeta`, `OpenMetaWithFS` and `ReadMeta` now return
  `(*mkv.Container, []DroppedTrack, error)` (was `(*mkv.Container, error)`).

### Fixed

- The MP4 probe no longer reports a QuickTime **chapter track** (referenced via
  `tref/chap`) as a dropped track — its content is already read from `chpl`.

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
