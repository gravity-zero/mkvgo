# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. This file
holds the unreleased changes and the latest release; an older version moves to
its series' file in [changelog/](changelog/) when a newer one ships.

## [Unreleased]

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
