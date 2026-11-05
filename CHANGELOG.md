# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

### Breaking

- Bytes served: HLS over CMAF adds the heaviest audio of a variant's group to
  its `BANDWIDTH`; a plan's DASH manifest lists the trick-mode set only when
  every segment has an I-frame, as the full pass does (it named fragments it
  then refused).
- `demux` to Ogg: a Vorbis page's granule is the end of its packet (it was the
  start), Opus's last one takes the end trim (DiscardPadding), and a packet
  past 65,025 bytes spans pages instead of failing.
- `Diagnose` with `DeepVerify` on a cut Segment of unknown size says
  `truncated` (tail), no longer `damaged` (body) with a `reindex --resync`
  remedy.
- `info` on an http(s) or s3 source leaves `truncated` out, as `probe` does:
  the check cost a request per top-level box of a fragmented MP4.
- The H.264 in-band scan reads each sample's first 16 KiB, as the MP4 one
  does: a slice header past it no longer settles `scan_type`.
- WebVTT: a BlockAddition's lines end at CR, LF or CRLF, so one written with
  CR alone keeps its cue identifier (served and extracted `.vtt`, `wvtt`).

### Fixed

- `demux`: a frame its codec's file cannot hold names the track, the block's
  time and `--raw`.

### Performance

- `demux` reuses one payload and one Annex-B buffer instead of allocating two
  per frame: half the time on a 1080p H.264 film.
- A CENC full pass reads each segment into a buffer its track keeps and
  encrypts it in place: about 40 % less time with `cbcs`.
- Writing a BlockGroup no longer copies its frame.

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
