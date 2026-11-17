# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

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

## [0.67.0] - 2026-11-17

### Added

- `subtitle.VobSubDecoder` and `subtitle.DVBSubDecoder` decode the bitmap
  subtitles of DVD rips (`S_VOBSUB`) and of broadcast captures (`S_DVBSUB`)
  block by block, as `subtitle.PGSDecoder` does PGS. Both return the PGS
  decoder's display sets (`subtitle.BitmapDisplaySet`, the same type): each
  picture in straight-alpha NRGBA with its position on the subtitle plane, an
  empty set where the screen clears.
- `subtitle.ParseVobSubIndex` with `VobSubIndex.ForEachUnit`, and
  `subtitle.ForEachSUPDisplaySet`, read the bitmap subtitles that sit beside a
  video: a VobSub `.idx`/`.sub` pair and a PGS `.sup`, one unit or display set
  at a time, each with its time, in the form the decoders take.
- `subtitle.PGSDisplaySet` gains `Delay` and `Duration`, which a VobSub unit's
  start and stop commands set, and `Timeout`, a DVB page's time-out; each is
  zero where the codec has no such thing.
