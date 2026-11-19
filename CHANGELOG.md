# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Fixed

- Stray bytes ahead of the first Cluster that happen to decode as an element
  header (the tail of an old tag an in-place edit before 0.56.0 could leave)
  no longer make `analyze`, `ingest -analyze` and `validate` report a complete
  file as cut short, or lose its first Clusters without a word: the block walk
  resynchronizes past them as the head reads do.

## [0.70.0] - 2026-11-19

### Changed

- Grid-timed audio whose timecodes run a frame ahead of the grid for a while
  is no longer described as experimental: measured in hls.js, Shaka and
  ExoPlayer, no player stalls on the window that absorbs the frame; Firefox
  keeps the audio up to about a frame late past it, as before 0.62.0
  (`docs/library.md`, HLS plan).

### Added

- `HLSPlanStats.SubtitleHarvestedClusters`: the clusters a subtitle rendition
  took its cues from as a window walk read them.

### Breaking

- Verdicts: `Playability` and `Ingest` send a PQ or HLG video with no
  mastering metadata or MaxCLL to transcode on a target without HDR (reason
  `HDR (PQ transfer) is unsupported on target`); it was direct-play before.
  They also read the first sample of a video track whose header states no
  colour, so a PQ stated only in the SPS counts.
- Bytes written: `PlanHLS` with `SynthesizeIndex` passes over a damaged region
  as the window walks do (`DamageBytes`) instead of ending the index there, so
  such a file's playlists change: the last window ran over the rest of the
  file (a 43 min episode with damaged regions planned 35 windows, the last
  served as one 2.2 GB segment; 309 now).

### Fixed

- `PlanHLS` without a subtitle index no longer walks the file again for each
  subtitle rendition: the cues come from the window walks, and a subtitle
  request builds the windows the video then serves from (over a network
  mount, half of what was asked of the source before). A 1080p episode
  played with one subtitle read 213 % of what was served, 113 % now (102 %
  without the subtitle); three subtitles cost what one does
  (`docs/library.md`, HLS plan).
- A window walk and a streamed segment no longer ask the disk to read ahead
  past the end of the file, and a remote source (`mkv.RangeHinter`) is no
  longer asked for an open-ended range: the plan's head peek and its last
  window hinted to `MaxInt64`.
- `AudioStartDelays` (and `Diagnose`) seek over what precedes the first
  cluster instead of reading it: the attachments, a subtitle's fonts often
  tens of MB, were read on every call. A cluster's declared size is no longer
  allocated before its bytes arrive (a 32-byte file declaring 200 MiB
  allocated 200 MiB).
- A block's declared size no longer allocates more than the bytes that arrive
  past 8 MiB (a 51-byte file declaring a 60 MiB block allocated 60 MiB on
  every walk that met it; 1 MiB now).
- `retime` and `reindex` no longer allocate a cluster's declared size before
  its bytes arrive (a 32-byte file declaring a 200 MiB cluster allocated it).
