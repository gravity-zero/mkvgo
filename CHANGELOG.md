# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Breaking

- JSON (CLI `-json`, WASM): an empty list is `[]` and an empty map `{}`, never
  `null`; the `cues[]` and `dropped_tracks[]` keys are snake_case
  (`time_ms`, `cluster_pos`, `id`, `reason`...). The Go API is unchanged.
- `Diagnose`: a Segment of unknown size (a streamed write) is a note in
  `Diagnosis.Notes`, no longer a finding; such a file is healthy.
- `--exit-damaged` also covers `info`, `probe`, `track-ends`,
  `salvage --dry-run` and `diagnose` (3 replaces its 1 on damage).
- `demux` writes each track as its codec's file (ADTS, Annex-B, Ogg, FLAC,
  `.sup`); `--raw` / `DemuxOptions.Raw` keeps the old bare payloads. See
  [cli.md](docs/cli.md#demux).
- Bytes served (inits, playlists, MPD): a cache keyed on them is stale.
  - Audio channels come from the stream (the AudioSpecificConfig, the AC-3 /
    E-AC-3 frame header, the init's sample entry), not the container; an
    E-AC-3 `dec3` lists its dependent substreams (a 7.1 says 8) and reads an
    AC-3 core as AC-3.
  - HLS masters: one audio group per codec when every language is in each,
    else one group; one `DEFAULT=YES` per group; unique `NAME`s; each
    variant's `BANDWIDTH` is its video and its group's heaviest audio track,
    the DASH video `bandwidth` the video alone. See
    [library.md](docs/library.md) (manifests).
  - New manifest attributes and elements (see Added): `VIDEO-RANGE`,
    `SUPPLEMENTAL-CODECS`, the CICP properties, `CHARACTERISTICS`, `Role` and
    `Accessibility`, Atmos (`CHANNELS="16/JOC"`, the `EC3_ExtensionType`
    property, the `dec3` extension), and the DASH trick-mode AdaptationSet
    (the video set gaining `id="1"`).
  - A Dolby Vision track in a Dolby sample entry is named `dvh1.PP.LL`
    (`dav1`, `dva1`) in `CODECS` / `codecs` (see Fixed).
  - Every audio and subtitle `EXT-X-MEDIA` carries `X-MKVGO-TRACK` (see Added).
  - An integrator caching masters, MPDs or inits built by an earlier version
    must rebuild them.

### Added

- `SourceCutShort` / `CutShortOf`: whether a source stops before its declared
  end, head-only (also the WASM probe's `truncated`).
- `Finding.Scope` (`head`, `body`, `tail`), the `mkv.Finding*` kind constants
  and `FindingKinds()`; `Track` on two more findings.
- `BlockReader.StoppedShort`, `TrackEndsReport.StoppedShort`.
- Manifests: `VIDEO-RANGE`, `SUPPLEMENTAL-CODECS` (Dolby Vision), CICP
  properties, `CHARACTERISTICS` / `Role` / `Accessibility`, Dolby Atmos (JOC)
  and a DASH trick-mode set over the I-frame fragments.
- HLS: `X-MKVGO-TRACK="<n>"` on every audio and subtitle `EXT-X-MEDIA`, the
  identifier the source file gives the track (Matroska TrackNumber, MP4 `tkhd`
  track_ID), to pair a rendition with its track without its `NAME`; the probe
  gives it as `Track.FileTrackID`. See [library.md](docs/library.md)
  (manifests).
- Block additions: `Block.Additions`, `Track.BlockAdditionMappings`,
  `Track.HDR10Plus`; every rewrite keeps them, WebVTT cue settings reach the
  converted WebVTT and `wvtt`.

### Changed

- Refusals carry their sentinel for `errors.Is` (`ErrNoKeyframeIndex`,
  `ErrNoVideo`, `ErrSourceTruncated`, `ErrDamagedRegion`, `ErrSourceChanged`);
  the messages change wording, match the sentinels.

### Fixed

- Cut and damaged files: `Diagnose` says `truncated` for a file cut between
  clusters, or one of unknown size cut inside a block; a cut tail is no longer
  damage; trailing tags or Voids after a streamed Segment are not a cut;
  `DeepVerify` on a streamed Segment reports its body damage; `Analyze`,
  `Validate` and `CueHealth` name the cut.
- Cue points past the end of a complete file are a corrupt index to reindex
  (`cues-stale`, `ErrNoKeyframeIndex`), not a cut source.
- MP4: `DeclaredSize`, `info` and `probe` walk every top-level box, as
  `Diagnose` does.
- A Dolby Vision track in a Dolby sample entry is named `dvh1.PP.LL` (`dav1`,
  `dva1`) in `CODECS`.
- Commands that read a whole local file refuse an http(s)/s3:// source with
  exit 2; documented JSON keys and `-json` list held to the code by a test.
- CLI: `demux`, `split`, `edit`, `edit-title`, `edit-track`, `remove-track`,
  `add-track`, `merge-subtitle`, `extract-subtitle`, `subtitle-index` and
  `extract-attachment` read flags placed before the source (`demux --raw f.mkv`
  opened a file named `--raw`).
- CLI: `info`, `probe`, `tracks`, `chapters`, `attachments`, `keyframes` and
  `tags` refuse an unknown flag with exit 2, before or after the file (it was
  opened as a file before it, ignored after it); global flags stay accepted
  anywhere.

## [0.63.0] - 2026-11-02

### Breaking

- Bytes written: a remux of a Matroska source that states a disposition flag
  as 0 (`FlagOriginal` 0, ...) leaves the element out, the spec default, and
  writes a flag set to 1 after `FlagForced`, where the source's own sat after
  its Video or Audio element; the media bytes are unchanged.

- `Track.ScanType`: an H.264 track whose SPS has `frame_mbs_only_flag` 0 and
  whose container states no `FlagInterlaced` now reads `""` (unknown), not
  `interlaced`: the flag only allows field coding, progressive streams carry
  it. A consumer that halved the frame rate on `interlaced` should treat `""`
  as unknown. `FlagInterlaced` 1 still gives `interlaced`.

- `Track.FieldOrder`: the Matroska `FieldOrder` values 9 and 14 now read `tb`
  and `bt`, the names the reference prober gives them (the first letter is
  the field stored first); mkvgo had them swapped. A `field_order` cached
  from an earlier release holds the swapped name: probe the file again.

### Fixed

- The disposition flags (`HearingImpaired`, `VisualImpaired`,
  `TextDescriptions`, `Original`, `Commentary`) are written from the `Track`
  fields: one set through the API is now in the output, one cleared is now
  left out (before, the source's elements were copied back as they were, and a
  flag set in code was lost).
- `Attachment.Description` (the Matroska `FileDescription`) is read, written
  when not empty, and kept by every operation that rewrites the attachments;
  before, a rewrite dropped it. `attachments --json` shows it.
- Hostile or damaged input, each refused or bounded where it panicked, looped
  or allocated what it declared: an `lpcm` entry shorter than its layout, a
  PCM table of 2^32-1 frames, a NAL, OBU or SEI length near 2 GiB and an HEVC
  syntax value past its range on a 32-bit build, an H.264 or HEVC crop past
  the picture, more than a million chapter atoms or simple tags, an MP4 sample past
  256 MiB, a VP9 superframe index short of its sample (`cenc`), a head element
  sized past the file (`edit-inplace`). `verify` hashes a sample as it reads
  it, no sample held whole.
- On-demand HLS of a Matroska source cut short after its head index declared
  segments past the file's end and a negative final duration, one cut inside
  a cluster lost the whole frames before the cut, and one cut inside the first
  block of a cluster served an empty last segment; the plan serves what the
  file holds, as the full pass does.
- `--keep-lang` and the wasm's `keepLangs` match the language the container
  resolves (a BCP 47 tag, the default English of a track stating none, `und`
  as written), where such tracks were dropped; a language matching no audio
  track is said.
- `probe --json` carries `truncated` for a local file, as `info --json` does.
- An AV1 sequence header with decoder model parameters keeps its colour
  config; a `BlockReader` given a TimecodeScale of 0 uses the default; MP3
  frame sizes are known under the Matroska CodecID.
- wasm: `codec_name` is written; the TypeScript types say the CICP colour
  fields are numbers and declare the analyze fields the JSON carries.
- `serve` prints the source without its query (a presigned URL's signature
  stayed on stdout).

### Added

- `matroska.ReadStream`: the forward-only reader on the facade; the CLI and the
  wasm go through the facade alone.
- The in-band option (`WithInBandColourFallback`, `mp4.Options.InBandColour`,
  always on in the remux and plan entry points) reads the first 16 samples of
  an H.264 track whose SPS allows field coding and gives it `interlaced` on
  proof - field-coded slices, frames whose fields have different picture order
  counts, or the picture timing SEI - with `FieldOrder` `tt`/`bb` when the
  samples agree; on MP4 too, and for an `avc3` record from the SPS the samples
  carry. An HEVC track whose hvcC states both source flags or neither is
  `interlaced` when its SPS codes every picture as a field, or, with the
  option, when its picture timing SEI says so. An MPEG-2 track gets its scan
  type from `progressive_sequence` and, with the option, from the picture
  coding extensions of its first samples. `reader.NewInBandScan` drives it off
  another sample table. See [docs/library.md](docs/library.md).

### Changed

- The 0.62.0 Deferred list no longer names the static release build, the full
  coverage and the TypeScript types, which that version shipped.

## Older versions

Archived in [changelog/](changelog/): [0.60-0.69](changelog/0.60-0.69.md), [0.56-0.59](changelog/0.56-0.59.md), [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
