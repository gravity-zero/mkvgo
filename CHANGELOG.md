# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Security

- A BlockAdditional of unknown size crashed demux, remux, the HLS plan and
  `info -in-band` on the file; it is refused as damage, and so is a
  BlockMore or BlockAdditions of unknown size and a BlockGroup with more than
  64 BlockMore or 16 MiB of additions. Upgrade.
- WebVTT: a cue's settings or identifier taken from its BlockAddition could end
  its line and time cues of its own in the served or extracted `.vtt`; they
  stop at their first line end, and one holding `-->` is dropped.
- 32-bit builds: a NAL length past its frame crashed `demux` and keyframe
  extraction; it is refused.
- 32-bit builds: an audio CodecPrivate holding a `dec3` box of a crafted size
  crashed every HLS and DASH manifest of the file; the Atmos (JOC) check now
  reads only an E-AC-3 sample entry, so no other track announces Atmos either.
- 32-bit builds: an AV1 CodecPrivate OBU of a crafted size dropped the track's
  colour information or read a wrong one; the OBU is ignored.

## [0.64.0] - 2026-11-04

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

## Older versions

Archived in [changelog/](changelog/): [0.60-0.69](changelog/0.60-0.69.md), [0.56-0.59](changelog/0.56-0.59.md), [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
