# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Breaking

- JSON (CLI `-json` and the WASM results): an empty list is `[]` and an
  empty map `{}`, where some reports gave `null` (`findings`,
  `damaged_ranges`, `repaired_ranges`, a Container's `chapters` and
  `attachments`, the `chapters`, `validate` and `compare` reports of a file
  with none); an optional section is still left out. A jq `.findings[]` or a
  JS `.map` no longer meets a null; a check for `=== null` must test the
  length. The Go API is unchanged (a nil slice stays nil).
- JSON keys: a cue point (`cues[]` in a Container's JSON) carries
  `time_ms`, `track`, `cluster_pos`, `relative_pos`, and a dropped track
  (`dropped_tracks[]`, CLI `probe --json` and the WASM remux results) `id`,
  `type`, `codec`, `reason`; they were the Go field names (`TimeMs`, `ID`,
  ...), the only CamelCase keys left. A test holds every Container key to
  snake_case.
- `Diagnose`: a Segment that declares no size (a streamed write) is a note in
  the new `Diagnosis.Notes`, no longer a finding: such a file is `Healthy` and
  `diagnose` exits 0. The `streamed-size` kind and its remedy are unchanged.
- `--exit-damaged` also applies to the commands that read: `info` and `probe`
  on a local file cut short, `track-ends` on one cut short (now said on
  stderr) or whose walk passed over damage, `salvage --dry-run` that maps
  damage or a missing tail, and `diagnose` with a `truncated`, `damaged` or
  `sample-table` finding, where 3 replaces its 1. Without the flag nothing
  changes. `probe` prints `Truncated` in its text output too.
- Bytes served: to-mp4, to-hls, the on-demand plan and the ABR/DASH packaging
  announce an AAC, AC-3 or E-AC-3 track's channel count from its stream (the
  AudioSpecificConfig, the first frame header) where the Matroska container
  states another: the AudioSampleEntry's `channelcount`, the HLS `CHANNELS`
  attribute and the DASH `AudioChannelConfiguration` now agree with the
  decoder configuration of the same init (a 5.1 stream declared 2 by its
  container was announced 2); an AAC config saying mono without signalling
  PS keeps a container's 2, an implicit PS decoding as stereo. Inits and
  playlists of such sources change; a
  cache keyed on them is stale. The packaging reads one block more per AC-3
  or E-AC-3 track at open.
- `demux` writes each track as the file its codec's tools read: AAC as ADTS,
  H.264 / HEVC as Annex-B with the parameter sets ahead of every keyframe,
  Opus and Vorbis in Ogg (`.opus`, `.ogg`, was `.vorbis`), FLAC with its
  STREAMINFO, PGS as `.sup` (was `.hdmv_pgs_subtitle`); they were the bare
  stored payloads, which no tool reads. `DemuxOptions.Raw` / `--raw` keeps
  the old output.
- Bytes served, HLS masters (`to-hls`, the plan, ABR, concat,
  `HLSFromCMAF`): audio tracks of different codecs form one group each
  (`aud-<codec>`) with one variant per group when every language is in every
  group (a player lists only its variant's group); otherwise one group `aud`
  keeps them all, its variant's `CODECS` naming each codec once. One
  `DEFAULT=YES` per audio group and at most one in the subtitle group (every
  track without FlagDefault read as default gave several); every `NAME` unique
  in its group (untitled tracks of one language get " 2", " 3"). Each
  variant's `BANDWIDTH` / `AVERAGE-BANDWIDTH` counts its video and the
  heaviest audio track of its group, the one a player may pick (was every
  audio track's); the DASH video Representation's `bandwidth` its video alone
  (was the audio too). See Added for the new attributes.
- Bytes served: an E-AC-3 track's `dec3` lists the dependent substreams of
  its first programme and their channel locations (a 7.1, a 5.1 and the rear
  pair, was announced 6), and one whose access units open with an AC-3 core
  takes that core's fscod, bsid, bsmod, acmod and lfeon where AC-3 puts them
  (they were read as E-AC-3 fields).
- Bytes served: the HLS `CHANNELS` and DASH `AudioChannelConfiguration` of
  an AC-3, E-AC-3 or AAC track come from the sample entry the init carries,
  also for a track whose first block lies past the in-band read's wait (they
  fell back on the container's count there, the init saying another).

### Added

- `SourceCutShort` / `CutShortOf` (ops and the `matroska` facade): whether a
  Matroska or MP4 source stops before the end it declares, head-only; the
  CLI's `truncated` object, now also in the WASM probe result.
- `Finding.Scope` (`head`, `body`, `tail`): where a defect of the file's
  bytes lies, so a `damaged` finding says whether media is lost in the middle.
  The Finding kinds are constants (`mkv.Finding*`, `FindingKinds()`, also in
  the `matroska` facade); every documented list now names all 16.
- `undeclared-compression` and `unsized-laced-audio` findings set `Track`.
- `BlockReader.StoppedShort`: the walk ended because the source stops inside
  an element.
- Manifests: `VIDEO-RANGE` (PQ, HLG) and `SUPPLEMENTAL-CODECS` (Dolby Vision
  8.x / 10.x) in HLS, the CICP colour properties in DASH; `CHARACTERISTICS`
  (HLS) and `Accessibility` / `Role` (DASH) from the disposition flags; Dolby
  Atmos (E-AC-3 JOC) read from the stream, carried in the `dec3` and announced
  (`CHANNELS="16/JOC"`, the DASH `EC3_ExtensionType` property). One set of
  helpers serves every HLS and DASH writer.
- Block additions: `Block.Additions` reads a BlockGroup's `BlockAdditions`
  (VP8/VP9 alpha, ITU-T T.35 / HDR10+, WebVTT cue settings), every rewrite
  writes them back (the block a BlockGroup), with the track's
  `BlockAdditionMappings`; `Track.HDR10Plus` from a T.35 mapping or, in-band,
  the additions. WebVTT cue settings reach the converted WebVTT and the native
  `wvtt` sample (`sttg`, `iden`). They were dropped by every rewrite. MP4 has
  no standard carriage for a VP9 alpha plane or VP9 HDR10+: not carried there.
- DASH trick play: `manifest.mpd` (to-hls and the on-demand plan) carries a
  DASH-IF trick-mode AdaptationSet over the I-frame fragments `iframe.m3u8`
  already lists, one per segment (`maxPlayoutRate`, `codingDependency="false"`),
  the video set taking `id="1"`; none under `NoIFrames` or encryption, nor in a
  full pass where a segment does not open on a keyframe.

### Changed

- Refusals carry the sentinel of their condition, for `errors.Is`:
  `ErrNoKeyframeIndex` (PlanHLS without Cues or without a video cue, Split
  every N without keyframes, ExtractKeyframeSample), `ErrNoVideo`
  (ExtractKeyframeSample, RecommendLadderFor, PlanConcat), `ErrSourceTruncated`
  (PlanHLS on a file ending before its first cued cluster, Analyze on a file
  cut inside a block), `ErrDamagedRegion` (Salvage past its resync scan),
  `ErrSourceChanged` (an I-frame that no longer matches its plan). The first
  two now live in `mkv` (the `ops` and `matroska` names are the same values).
  The messages change wording: match on the sentinels, not the text.

### Fixed

- The commands that read the whole file from a local disk (`cue-health`,
  `track-ends`, `diagnose`, `validate`, `verify`, `hash`, `compare -blocks`,
  `salvage`) refuse an http(s)/s3:// source with exit 2 and the commands
  that take one (was a bare "no such file" open error).
- Docs: the `-json` command list (`verify` has a JSON report, `hash` does
  not), the jq examples (snake_case keys), `validate` on non-video cues,
  the remote-capable commands, `diagnose`'s JSON keys and its MP4 usage,
  `CompareContainers`, `TrackEndsReport.SkippedBytes`, the TypeScript
  disposition flags; a test now holds the `-json` list and the jq keys to
  the code, and the usage lines come from one table.
- On a file cut short: `Analyze` adds a warning that its stats cover what
  arrived (MKV and MP4; an MP4's totals were silent); `Validate` reports the
  cue points naming clusters past the end as `cues-past-end` (was
  `cues-stale` with a reindex remedy); a `CueHealth` verdict's reason names
  the cut and the re-download before any reindex.
- `Diagnose` reports a file cut exactly between two clusters as `truncated`
  (it said `healthy`, with `missing_tail_bytes` set in the same report); the
  declared Segment size alone decides it.
- A file cut inside a block: `TrackEnds.SkippedBytes` no longer counts the cut
  tail, so `Diagnose` stops reporting it as damage with media behind it.
- A Matroska whose Segment declares no size (a streamed write) and that stops
  inside a block: `Fingerprint` refuses it with `ErrSourceTruncated` (was a
  bare "unexpected EOF"), and to-mp4 / to-hls report the cut through
  `OnTruncated` (they were silent). Such a Segment whole and followed by
  bytes that are no Matroska element (an ID3 tag, a Void running past the
  end) is not cut: only an end inside a sized Cluster, a block or a
  top-level element counts.
- A Matroska whose Segment declares no size and that stops inside a block:
  `Diagnose` reports it `truncated` (tail) and `track-ends` warns of the cut
  and exits 3 under `--exit-damaged` (both took it for whole since the cut
  tail stopped counting as damage); `TrackEndsReport.StoppedShort` says so.
- `Diagnose` with `DeepVerify` on a Segment of unknown size: the Segment runs
  to the end of the file, so its damaged regions are reported (the whole file
  was counted as trailing junk and its body damage dropped).
- Cue points naming positions past the end of a file that holds its whole
  Segment are a corrupt index, not a cut: `Validate` reports them as
  `cues-stale` with a reindex (was `cues-past-end`, "re-download"), and
  `PlanHLS` refuses with `ErrNoKeyframeIndex` (was `ErrSourceTruncated`). A
  video cue among them no longer leaves `cues-non-video` saying every cue
  references a non-video track.
- MP4: `info`, `probe`, `SourceCutShort` and `mp4.DeclaredSize` walk every
  top-level box as `Diagnose` does, so a fragmented file cut short is said
  cut by both (past 16 boxes they left it unjudged while `Diagnose` said
  `truncated`).
- Manifests: a Dolby Vision track carried in a Dolby sample entry (`dvh1`,
  `dav1`, `dva1`: a base layer that is not cross-compatible, profile 5 or 7)
  is named so in HLS `CODECS` and DASH `codecs`, `dvh1.PP.LL` (was the plain
  `hvc1`/`av01`/`avc1` string, a codec the init does not carry).

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
