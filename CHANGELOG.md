# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

## [0.66.1] - 2026-11-08

### Fixed

- HLS plans (`serve`, `Open`): a chmod, a chown or a hard link of the source
  no longer counts as a changed source. 0.66.0 had added the inode change
  time to the source stamp, so any of them made every later segment of a
  running serve fail with `ErrSourceChanged` (HTTP 502) until the process was
  restarted, and a player gave up after its retries. The stamp is the size,
  the mtime and the file identity again; a file rewritten in place at the
  same size with its mtime put back is, as before 0.66.0, not detected.

## Older versions

Archived in [changelog/](changelog/): [0.60-0.69](changelog/0.60-0.69.md), [0.56-0.59](changelog/0.56-0.59.md), [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
