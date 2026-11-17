# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Added

- `DamagedRange.Err` (`mkv`, `matroska`): set on the range `Options.OnSkip`
  gets for a bitmap subtitle block that was read but did not decode, to the
  decoder's error; nil for a damaged region. `extract-subtitle` names such a
  block on stderr by its time and that error, in a message of its own.

### Fixed

- `ForEachSubtitleBitmap`, `ForEachSubtitlePGS`, their `From` forms and
  `extract-subtitle -format png|pgs`: a block that reads but does not decode
  no longer fails the whole track. It is lost alone, the cue on screen ending
  at its time, and reported through `Options.OnSkip` (`DamagedRange.Err`
  says why); the rest of the track is delivered. `Options.DamageBytes` negative (`--damage-bytes -1`) still fails
  the read on it. A PGS track with such a block failed since PGS extraction
  exists.

## [0.68.0] - 2026-11-17

### Added

- `ForEachSubtitleBitmap` and `ForEachSubtitleBitmapFrom` (`matroska`,
  `mkv/ops`) give a bitmap subtitle track as timed pictures whatever its codec,
  PGS, VobSub or DVB, walking the file or served from a `SubtitleIndex`;
  `BitmapCue` is the type `PGSCue` names. `ForEachSubtitlePGS` and its
  siblings are unchanged.
- `ForEachSubtitleBlock` and `ForEachSubtitleBlockFrom` give the blocks of a
  subtitle track of any codec (`SubtitleBlock`: time, duration, decoded
  payload), walked or served from an index under the checks of the other
  indexed reads, a damaged region passed over as a `Lost` call.
- `extract-subtitle -format png` writes any bitmap track (PGS, VobSub, DVB) as
  PNGs and a `cues.json` manifest, with `-index` too; `-format pgs` is
  unchanged.
