# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

## [0.71.1] - 2026-11-20

### Fixed

- Under `WithInBandColourFallback` (`-in-band`, `mp4.Options.InBandColour`),
  three video tracks whose header names no colour now get it from their first
  sample, where they kept none or the wrong "determined": a VP9 track whose
  CodecPrivate holds the Matroska feature records, or whose Colour element
  states the range alone (the keyframe header was read only when the profile
  was missing); an AV1 track whose `av1C` holds no sequence header (a PQ
  video read as having no colour); an HEVC track whose `hvcC` holds arrays
  but no SPS. A VP9 Matroska file with no colour anywhere costs one more
  bounded read under the option.
- A VP9 keyframe header that says its colour is unknown sets
  `ColourDetermined`, as an H.264, HEVC or AV1 stream stating "unspecified"
  does: the colour was read, there is none to find elsewhere.
