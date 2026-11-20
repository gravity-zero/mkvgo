# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Fixed

- A VP9 track whose CodecPrivate holds the Matroska feature records (profile,
  bit depth, chroma, no colour) and that has no Colour element gets its colour
  from the keyframe header under `WithInBandColourFallback`: the header was
  read only when the profile was missing, so the colour stayed undetermined.

## [0.71.0] - 2026-11-20

### Fixed

- Stray bytes ahead of the first Cluster that happen to decode as an element
  header (the tail of an old tag an in-place edit before 0.56.0 could leave)
  no longer make `analyze`, `ingest -analyze` and `validate` report a complete
  file as cut short, or lose its first Clusters without a word: the block walk
  resynchronizes past them as the head reads do.
- A Void or CRC-32 ahead of the first Cluster declaring a few bytes too many
  no longer swallows the head of the element behind it: the readers
  resynchronize on that element instead of losing it without a word.
- `reindex --resync` and `salvage` keep the Info and Tracks that sit behind
  such bytes (the output had no track, exit 0), and `keyframes -walk` no longer
  loses the keyframes of the Clusters a stray size led it past.
- A few stray bytes between the last Cluster and the Cues or Tags closing a
  finished file (up to 64) end the block walk as the end of the media: such a
  file is no longer reported as cut short by `analyze`, nor refused by
  `demux`, `split`, `remove-track` and `fingerprint`. A longer run is still
  refused: it may hide a Cluster.
- `diagnose` no longer answers healthy when stray bytes sit between the head
  elements it reads and the first Cluster: it walks the head to the first
  Cluster.
- The strict `reindex`, `reindex-inplace` and `retime` refuse bytes that read
  as an element running past the end with the offset and the command that
  works (`mkvgo reindex --resync`), not `copy unknown 0x8C body: EOF` nor a
  "truncated file" pointing back at the command that refused.
- Bytes that read as a Cluster header too small to hold a block (8 bytes at
  most, opening with no element of a Cluster) are passed over as the stray
  bytes they are instead of refused as a damaged region, and `diagnose` counts
  them.
- `analyze` warns when the file it measured was read past undecodable bytes.
- What a strict block walk now reads past without an error, where it failed
  or lost media before: bytes ahead of the first Cluster that decode as an
  element whose size leads nowhere or that declare no size, the excess of a
  Void or CRC-32 that declares too much, 8 bytes at most read as a Cluster,
  64 at most before the closing elements. `Container.ResyncedBytes`,
  `validate`, `diagnose` and `analyze` say so for the head; a tolerant walk
  counts them all (`SkippedBytes`). `ReadStream` cannot look past an element
  and keeps skipping a header that decodes by its size.
- Bitmap subtitles: a block whose display sets together composite more than
  two subtitle planes is passed over as damaged (each composition had its own
  picture, 64 MiB for 30 bytes of block); a PGS object's buffer grows with
  its data instead of taking the 4 MiB its first segment may declare; a nil
  `SubtitleIndex` refuses `MarshalBinary` instead of panicking.
- Bitmap subtitles, again: a PGS block hands back two planes of pictures at
  most, not four (256 MiB for 115 bytes of block); a VobSub control sequence
  with no end command stops where the next one starts instead of running its
  header as commands (a picture forced on, at the wrong time); `extract-subtitle
  -format png` encodes a picture shown again once, each cue keeping its file.
- `to-mp4`, `to-webm`, `to-hls`, `demux`, `split` and `remove-track` warn when
  the Matroska source was read past undecodable bytes between its elements,
  and exit 3 under `--exit-damaged`: they said nothing.

### Changed

- A VobSub header with no `palette:` line (an old `.idx`, whose palette is in
  the `.ifo` beside it, or a track muxed from one) is read instead of refused:
  each picture is drawn in greys from black to white in the order of the pixel
  values it shows. A header with no `size:` line is still refused, and the
  refusal no longer speaks of a CodecPrivate for a `.idx` file.

### Added

- `Diagnosis.ResyncedBytes` (`resynced_bytes` in JSON): the bytes the head
  read passed over ahead of the first Cluster, so a caller needs no second
  read to count them. `WithWholeHead` (`reader` and `matroska`) gives the
  same count to a head read of its own. A Void declaring too much counts for
  its excess.
