# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

## [0.64.1] - 2026-11-05

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

## Older versions

Archived in [changelog/](changelog/): [0.60-0.69](changelog/0.60-0.69.md), [0.56-0.59](changelog/0.56-0.59.md), [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
