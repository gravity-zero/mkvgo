# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

## [0.65.1] - 2026-11-07

### Fixed

- Matroska: the H.264 in-band scan walks the whole block again (at most 1,024
  NALs, 16 KiB of headers parsed), so a block holding both fields of a frame,
  its first one past 16 KiB, keeps its `field_order` (0.65.0 lost it).

## Older versions

Archived in [changelog/](changelog/): [0.60-0.69](changelog/0.60-0.69.md), [0.56-0.59](changelog/0.56-0.59.md), [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
