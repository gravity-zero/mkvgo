# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

## [0.66.2] - 2026-11-10

### Security

- `serve`, `PlanHLS`: a Matroska whose Segment declares more bytes than the
  file holds (a forged size, a file cut short) no longer has a subtitle
  request ask the kernel to read ahead over the declared size, one call per
  chunk, with the rendition locked meanwhile (since 0.66.0).
- `edit-inplace`: a Tags, Chapters or Attachments element after the clusters
  that declares more bytes than the file holds is voided up to the end of the
  file; the edit wrote its declared size, from one allocation of that size.
- `demux`: an empty ALAC packet is refused by name (`--raw` keeps the stored
  payloads); a few bytes of laced empty packets held 2 KiB of packet table
  each (since 0.66.0).

### Fixed

- `edit-inplace`: a second edit of a file whose Info or Tracks an earlier
  edit had moved to the end of the Segment replaces them. It appended a
  second copy behind the first, which a full read kept returning: the edit
  reported success and was not seen (since 0.66.0). A file edited twice by
  0.66.0 or 0.66.1 is repaired by `mkvgo edit` (full rewrite).
- `to-hls`, `serve` and the other HLS commands: an encrypted text subtitle
  track is refused by name, or dropped with its reason under
  `--skip-unsupported`, as `to-mp4` does; since 0.66.0 it was listed as a
  WebVTT rendition built from its undecrypted bytes.
- `serve` on a remote source with a subtitle index: the scan asks for each
  block it reads again, not for the whole minute of media ahead in one request
  (since 0.66.0).
- Every command names `--strict`, `--skip-unsupported`, `--synthesize-index`
  and `--no-iframes` in its refusals; all but `demux`, `to-mp4`, `from-mp4`
  and `to-hls` printed the library's `Options` field names.
