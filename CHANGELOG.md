# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

## [0.69.0] - 2026-11-17

### Added

- `ForEachSubtitleBitmapFile` (`matroska`, `mkv/ops`) gives a bitmap subtitle
  file that sits beside a video - a PGS `.sup`, or a VobSub `.idx`/`.sub`
  pair - as cues, timed and ended as `ForEachSubtitleBitmap` does a track's,
  a unit that does not decode lost alone. `extract-subtitle` reads such a file
  as `-format png`, `-t` picking a `.idx`'s language from 1.
- `DamagedRange.Err` (`mkv`, `matroska`): set on the range `Options.OnSkip`
  gets for a bitmap subtitle block that was read but did not decode, to the
  decoder's error; nil for a damaged region. `extract-subtitle` names such a
  block on stderr by its time and that error, in a message of its own.

### Breaking

- Bytes written: `from-mp4` of a fragmented MP4 without `mehd` writes the
  Matroska `Duration` the probe now reports, the end of the latest track - a
  track ending where it starts plus what its samples last. A video track that
  starts after zero (a composition offset, no edit list) and outlasts the
  audio was left out, the duration being the longest sum of sample durations
  (2.763 s for a file whose last frame ends at 2.803 s; the probe of 17 of
  the 732 files of the public test corpora changes, none shortening). A
  track's own duration does not change.
- `subtitle.DVBSubDecoder` and `subtitle.PGSDecoder` keep the picture of a
  region or an object from one display set to the next, until its pixels or
  its colours change: display sets that show the same picture now share one
  image, as its placements within a set did, so a caller that wrote into a
  returned image changes the next sets' too - treat the images as read-only.
  A display set that only names an unchanged region again no longer reads it
  through its colour table again (a 14-byte block made a 4096x4096 region be
  rendered whole).

### Fixed

- A bitmap cue whose block states a duration ends no later than its DVB
  page's time-out; the block's duration won.
- `ForEachSubtitleBitmap`, `ForEachSubtitlePGS`, their `From` forms and
  `extract-subtitle -format png|pgs`: a block that reads but does not decode
  no longer fails the whole track. It is lost alone, the cue on screen ending
  at its time, and reported through `Options.OnSkip` (`DamagedRange.Err`
  says why); the rest of the track is delivered. `Options.DamageBytes`
  negative (`--damage-bytes -1`) still fails the read on it. A PGS track with
  such a block failed since PGS extraction exists.
