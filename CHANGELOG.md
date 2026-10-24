# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/). An entry says in a few lines what
changes for the user and what to do; the detail lives in the docs. When a
series of minor versions closes (0.6x at 0.70.0), its versions move to
[changelog/](changelog/).

## [Unreleased]

## [0.61.0] - 2026-10-24

### Breaking

- CENC on H.264/HEVC leaves parameter sets, SEI and slice headers clear, as
  ISO/IEC 23001-7 and CMAF ask; only slice data is encrypted. Every CENC
  H.264/HEVC segment changes: older ones still decrypt, but empty caches of
  them. Streams whose slice headers cannot be read (FMO, multi-layer HEVC) are
  refused. Rules and what was checked: [library.md](docs/library.md#common-encryption-cenc).

### Changed

- CLI: without `--cenc-iv`, `to-hls`, `to-abr`, `concat-hls` and `serve` draw
  a random IV and print it on stderr; `hls-segment`, `abr-segment` and
  `concat-segment` require it. The library still takes the IV it is given.

### Fixed

- A forged MP4 table count (stco, co64, stsc, stts, ctts, VP9 superframe
  index) no longer panics on 32-bit targets.

### Documentation

- README: head-only probing of an MKV without SeekHead or Cues reads every
  cluster header; a CENC key and IV must be unique per title.
- No package is described as stable or experimental any more: until 1.0 any
  package may change between minor versions, and Breaking names each change.
- Versions before 0.56.0 move to [changelog/](changelog/); entries are kept
  to a few lines, the detail lives in the docs.

## [0.60.0] - 2026-10-22

### Added

- `matroska.ErrMPEGTS` (and `mkv.ErrMPEGTS`): the error `mp4.ErrMPEGTS` is,
  now returned by every reader. An MPEG transport stream or a Blu-ray M2TS
  handed to the Matroska reader (`Open`, `OpenMeta`, `ReadStream`), to the
  content dispatch (`mkvgo.Diagnose`, `EditInPlace`) or to the repairs
  (`Reindex`, `Salvage`) is refused as "MPEG-TS detected: mkvgo does not read
  MPEG transport streams; remux the file to MP4 or Matroska first", whatever
  its name. They answered "ebml header: expected EBML header (0x1A45DFA3), got
  0x4740", or for an M2TS "invalid element ID: 5-octet VINT exceeds 4-octet
  limit", and the dispatch missed M2TS. It is not `ErrNotMatroska`, which sends
  a caller to the MP4 reader. The dispatch now asks three packets in a row, as
  the MP4 reader does, where it took two.
- `serve --memory-limit <MiB>` (off by default) sets the Go runtime's soft
  memory limit, as `GOMEMLIMIT` does. The window budget bounds the bytes kept,
  not the peak: the collector lets the heap grow to about twice what is live,
  kept windows included. At the budget plus 64 MiB, 100 random requests a
  second on a remote source peaked at 130 MB (MP4) and 142 MB (Matroska)
  instead of 137 and 191, at the same CPU per request.

### Fixed

- `hash` picks its engine from the file's first bytes, as `diagnose` does,
  not from its extension. A Matroska named `.mp4` is hashed instead of being
  told to remux, an MP4 named `.mkv` gets the MP4 answer (`to-mp4 --hash`)
  instead of "not Matroska/WebM", and an MPEG-TS or M2TS named `.mp4` is
  refused as MPEG-TS instead of getting the MP4 answer.

## [0.59.1] - 2026-10-22

### Fixed

- `RemuxToHLS` (`to-hls`) read each keyframe twice since 0.59.0: once for its
  segment, then back from the segment just written for its I-frame fragment,
  2-3 % of the source read again. It reads it once for both; the files written
  are unchanged.

## [0.59.0] - 2026-10-22

### Breaking

- Trick play: each entry of `iframe.m3u8` is a fragment of its own,
  `iframeNNNNN.m4s` (the I-frame of `segNNNNN.m4s`), holding the segment's
  leading keyframe alone behind a moof listing it alone, instead of an
  `EXT-X-BYTERANGE` into the segment. A byte range handed a player a trun
  listing every sample of the segment with only the first one present, and
  Chrome's Media Source refused the second I-frame appended ("Failed to
  prepare video sample for decode"): trick play showed one frozen image.
  `RemuxToHLS` writes these files beside the segments (a keyframe's size
  each, unless `NoIFrames`); a plan serves them through `Resource` and
  `Open` (one read of the keyframe from the source) and lists them in
  `Resources()`, a Matroska plan once `iframe.m3u8` was built. A server that
  routes by name must hand these names to the plan. The master's I-frame
  `BANDWIDTH` follows: `to-hls` and an MP4 plan declare the fragments' exact
  peak; a Matroska plan, whose master comes before any keyframe walk, keeps
  declaring an upper bound, the bit rate of its peak segment.

### Fixed

- A Matroska plan's `iframe.m3u8` for a video track stored compressed
  (zlib, bzlib, LZO) sized each I-frame from its compressed block; it reads
  the blocks to size them inflated, as the full pass does.

## [0.58.0] - 2026-10-22

### Added

- `mp4.ErrMPEGTS`: an MPEG transport stream or a Blu-ray M2TS handed to the
  MP4 reader (a `.ts` renamed `.mp4`, say) is refused as such, "MPEG-TS
  detected: mkvgo does not read MPEG transport streams; remux the file to MP4
  or Matroska first", instead of "not an MP4/MOV file"; the error is still
  `mp4.ErrNotMP4` for `errors.Is`.
- `HLSPlanStats.WalkReadBytes` and `CopyReadBytes` split `SourceReadBytes`,
  which stays their sum: what reading windows took (a Matroska window's walk,
  its reads ahead or far, foreign-block checks, subtitle cues) and what
  writing segments took (a streamed segment in the clear or AES-128, a CENC
  segment's samples, an MP4 segment built whole, a remote MP4 window's shared
  read). An MP4 source has no walk. The three count bytes asked of the
  source, not physical reads: a local Matroska segment's copy re-reads from
  the page cache what its walk read, and counts it again.

### Changed

- A remote MP4's video segment reads its window's span with the audio stored
  in it, in one request, and keeps that audio, every rendition, for the
  window's audio segments: within the window budget (`SetWindowBytes`), for
  30 s, and only until every rendition was served. An audio segment asked
  within 10 ms of its window's video read waits for it. On a NAS's MP4s
  behind an HTTP origin, 16 sequential viewers pull from the origin 1.02 to
  1.07 times the file's size for files interleaved frame by frame (2.0 to
  2.9 times before) and 1.09 times for files stored in chunks (1.23); with
  one audio rendition, an audio segment asked 10 s after its video still took
  no request of its own.
  A window whose audio lies away from its video (a span over 1.5 times the
  video's) is read as before. Costs: an audio segment asked alone waits up
  to 10 ms for a video read that may come (the 99th percentile of isolated
  audio at 100 requests per second from 6.8 to 15.4 ms; a local source waits
  nothing), and under requests that never take every rendition of a window
  the kept audio fills the window budget: the budget bounds the bytes kept,
  and the process's peak can reach about twice the budget over the rest
  unless `GOMEMLIMIT` bounds it (64 MiB budget, random requests: 41 to
  153 MiB). `KeptSegments` and `KeptFallbacks` count these windows too.

## [0.57.0] - 2026-10-21

### Breaking

- `mkvgo serve` answers `503` with `Retry-After: 1` and
  `Cache-Control: no-store` to a segment request
  past 64 in flight and 1024 waiting (`--max-in-flight 0` restores no cap).
- `mp4.SetWindowBytes` defaults to 64 MiB (it was 256 MiB), and only a
  network source's walks keep segments (a URL, a network mount,
  `SourceAccess: SourceNetwork`); a local file's are copied from the file.
- `HLSPlanStats.KeptFallbacks` counts only the windows a network source's
  walk built headers only because the window budget refused them; a local
  source, which never keeps, a keep turned off (`SetWindowBytes(-1)`) and a
  window of unknown span no longer count.
- `mkvgo serve` on a URL serves media segments with a weak ETag (size,
  SourceID, name) instead of the SHA-256 of their bytes, and one range at a
  time, as it already did for a local file.

### Added

- `mkvhttp.Options.MaxInFlight` and `MaxWaiting` bound the resources a
  handler serves at once and the requests queued behind them; one past both
  is answered `503` with `Retry-After: 1` and `Cache-Control: no-store`.
  `mkvgo serve` sets 64 and 1024
  (`--max-in-flight`, `--max-waiting`). Past what the source could feed, every
  waiting request held its buffer: at twice the capacity of a local file the
  heap reached 1.3 GiB in 20 seconds and the 99th percentile 4 s; it now
  stays at 130 MiB, with a 99th percentile of 0.6 s.

### Changed

- A video segment's bytes are asked of the disk 8 MiB ahead of its copy,
  instead of its whole range before the first byte: a cold 1080p 20 Mbit/s
  video segment comes in 3.9 ms instead of 5.1 from a fast disk; from one
  capped at 250 MB/s, as fast as before, with the 99th percentile under 16
  clients at 200 ms instead of 300 to 390.
- AES-128 and CENC segments are served in as many slots as there are
  processors, the oldest request first, each holding its slot for all its
  work but its writes to the client (a slow client holds none), so that under
  load they finish in turn: with 16 clients on 2 cores the 99th percentile
  falls from 225 to 155 ms for AES-128 and from 135 to 65 ms for CENC, for 10 %
  (AES-128) and 7 % (CENC) fewer requests per second; unencrypted serving is
  untouched.
- A remote MP4's audio segment is asked of the origin by its samples' ranges,
  holes under 64 KiB read through, when that takes 16 requests or fewer: the
  video between them is no longer fetched. A source storing its audio sample
  by sample between video frames (147 ranges a segment on a 1080p ffmpeg
  encode) is still read through in one request, each range otherwise a round
  trip (measured at 10 ms a request: 200 ms a segment by ranges, 28 ms read
  through).
- A local Matroska file no longer keeps the segments its window walks read:
  the copy comes from the page cache the walk just filled, so keeping them
  saved no disk read (16 sequential viewers read 1,387 MiB from disk either
  way) while a burst of first requests filled the window budget and the
  collector doubled it. With 16 clients on a 1080p 20 Mbit/s file, peak
  memory falls from 545 MiB to 30 MiB, and from 600 MiB to 75 MiB with 256;
  a cold video segment is served in 6.9 ms instead of 8.6, a cold audio one
  in 4.9 ms instead of 6.5, a sequential viewer's step in 28 ms instead of
  44 for a third less CPU, and the 99th percentile with 256 clients falls
  from 415 ms to 195 ms.
- The window budget (`mp4.SetWindowBytes`, `--window-bytes`) defaults to
  64 MiB: over an HTTP source it reads the origin as 256 MiB did (1.4 times
  the video served, against 2.4 times with nothing kept) and serves as many
  requests, for a peak of 175 MiB instead of 580 MiB with 16 clients.

### Fixed

- `mkvgo serve`'s default `--max-walks` is the processors the process may use
  (`runtime.GOMAXPROCS(0)`, which follows a container's CPU limit when built
  with Go 1.25 or newer) instead of the host's cores (`runtime.NumCPU()`).
- `httpfs` and `s3fs` keep 64 idle connections per host instead of the
  2 of Go's default transport: under 16 clients a remote source opened a new
  connection for 37 % of its requests, now 14 in all.
- `mkvgo serve` on a URL held each media segment whole per request, hashed
  its bytes for a strong ETag and built it with no cap: at twice its capacity
  the process grew past 15 GiB in eight seconds and was killed. Its segments
  now stream from the origin, as a local file's do, with the same bytes: on a
  1080p 20 Mbit/s MP4 behind a loopback origin, 16 clients get 2.4 times the
  requests per second for 2.6 times less CPU per request, and peak memory
  falls from 325 MiB to 45 MiB. A Matroska source serves 2.5 times the
  requests and now holds the kept-window budget (`--window-bytes`, 64 MiB by
  default).
- An MP4 source's segment built whole (`Resource`, a converted track, or a
  plan that does not stream) takes a slot of `mp4.SetMaxConcurrentWalks`, as a
  Matroska window does: the cap now bounds the segments held while they are
  built.

## [0.56.2] - 2026-10-20

### Changed

- mkvgo builds with Go 1.24 or newer (it required 1.27), the oldest release
  with the `weak` package it uses; CI tests 1.24, 1.26 and the latest. A
  large element's read grows its buffer by its own rule, under 1.5 times its
  size on every Go release.

### Fixed

- A Segment ending with elements outside Matroska's IDs (two in a row, one
  followed by a Void, or one closing a Segment of unknown size) is read as sound:
  `Container.ResyncedBytes` counted them as junk, so `Validate` reported
  undecodable bytes and named `reindex --resync` (a regression of 0.56.0).
- A TrackEntry without a TrackNumber, or with TrackNumber 0, is no longer
  published as a track by `Read`, `ReadMeta` and `ReadStream`: the number is
  mandatory and never 0, so no block can belong to it. `Read` could report
  such a track, with no type and no codec, from a Tracks element it found
  while resynchronizing through junk.

## [0.56.1] - 2026-10-20

This release adds exported API (`mkv.Container.SegmentEnd`,
`reader.BlockReader.SetSegmentEnd`): by semantic versioning it should have been
v0.57.0. Published versions are never retagged; from the next release on,
`make preflight` refuses a version gorelease does not ask for.

### Added

- `mkv.Container.SegmentEnd` states where the Segment's declared size ends;
  `reader.BlockReader.SetSegmentEnd` stops a block walk opened at an offset
  there.

### Fixed

- `Diagnose` reports bytes after a sealed Segment's end as trailing junk only:
  its walk from a cue passed them over as damage inside the file too, so a
  sound file with a tail of junk read as damaged and was sent to
  `reindex --resync` (a regression of 0.56.0). The block walks opened at an
  offset stop at the Segment's end (`Container.SegmentEnd`,
  `BlockReader.SetSegmentEnd`).
- An on-demand HLS plan stops its walks at a sealed Segment's end: junk past it
  no longer counts as damage, refuses the last segment under a small
  `DamageBytes`, nor weighs in the master's `BANDWIDTH`; a file whose junk
  read as an invalid element, which `PlanHLS` refused, is planned and served.

## [0.56.0] - 2026-10-20

Every version from v0.13.0 to v0.55.1 is retracted (see Security): upgrade to
this release, re-package what CENC 'cenc' encrypted and rotate its keys.

Highlights: split then join gives the source back sample for sample (in
Matroska, an open GOP's leading pictures aside); MP4 and MOV are edited
natively (split, join, mux, merge, in-place metadata) without a round trip
through Matroska; CENC packaging is hardened; a subtitle cue without a duration
ends once read, by one rule; on-demand serving counts what it really sends, and
a growing plan serves while it scans.

### Breaking

API:

- `GrowingHLSPlan.Complete` returns an error instead of swallowing it: a tail
  it cannot read closes the playlist on the segments published and says so, a
  source it cannot reach leaves the playlist open for another call. A call
  statement compiles as before; a method value of type `func()` does not.
- `mkv.CompressionFromAlgo` of a `ContentCompAlgo` Matroska does not define
  returns `CompressionUnknown`, no longer `CompressionNone`, and a writer
  refuses such a track; `Track.Type` names the complex, logo, buttons, control
  and metadata tracks where it was empty.
- `httpfs`: `Stat().Name()` no longer holds the URL's query or credentials,
  and `Rename` is refused.

Defaults:

- `serve` and `serve-growing` listen on `127.0.0.1:8478` by default;
  `-addr :8478` opens them to the network. Both time out slow clients (10 s
  header, 2 min idle, 5 min HLS response).
- `mp4.CENCOptions` without a `KeyURI` is refused instead of writing the key
  into the playlists as a `data:` URI; `InlineKey` (wasm `inlineKey`) asks for it.
- `s3fs` refuses to send a session token to an `http://` endpoint unless
  `Options.AllowHTTPSessionToken` is set; the refusal names the way out a CLI
  user has, an `https://` endpoint or unsetting `AWS_SESSION_TOKEN`.
- A negative `DamageBytes` in `mp4.Options` refuses any damage; it allowed the
  64 MiB default.
- `demux` refuses an output name that is taken unless `-f`.

Refused where 0.55.1 went ahead:

- `-segment` takes at least 0.001 and at most 86400 seconds on every command:
  `1e300`, `NaN` or `0.0004` is refused by name instead of wrapping to a
  negative length or falling back to the default.
- CENC: 'cenc' with two tracks whose IDs are equal modulo 255, an ABR ladder
  past 256 variants, a 16-byte IV, a repeated decode time or one past 2^48
  ticks; CENC on a concatenated presentation; an AV1 frame of several tiles.
- MP4: more than 16M samples in all, a segment past 256 MiB, a probe, subtitle
  or first-frame sample past its bound, a track switching sample descriptions
  for a remux or HLS.
- Matroska: Cues past 8M points (damage), a PGS display set whose objects lie
  more than 4096 pixels apart (damage), an `EditInPlace` or `ReindexInPlace`
  head whose sizes run over the clusters, `ReindexInPlace` on bytes past the
  Segment's end.
- Remote sources: an `s3fs` key segment `.` or `..`, an https to http
  redirect while `httpfs` `Options.Header` is set.
- A CMAF representation ID that is not a plain name, or is `master`.
- Split: a range ending before it starts or starting past the end, a source
  cut before its declared end, a part named like its source, sound stored more
  than 64 MiB past a cut keyframe, a bad name pattern. Join: tracks that do not
  pair (see Fixed).
- SRT extraction of a bitmap track; a negative SRT timestamp.
- The `edit` JSON patch with an unknown field or track.
- Any writer given one of its inputs as output; `--keep-backup` when the
  `.bak` exists.
- `ExtractSubtitleWebVTT` on a Matroska source cut before its declared end
  writes the cues it holds and returns `ErrSourceTruncated`; WebAssembly
  `extractSubtitleVTT` rejects with them in the error's `partial`.

Output and HTTP:

- `mkvhttp` answers 404 for a name a plan does not serve (an unknown name, a
  segment or rendition out of range), 502 only when the source fails.
- DASH writes a URI's `&` as `&amp;`; HLS percent-encodes `"`, CR and LF in a
  URI and drops them from a name.
- Subtitle cue ends (SRT, ASS, WebVTT, MP4 remux, HLS), and WebVTT line ends,
  change (see Changed and Fixed).
- A Matroska split part starts at 0 on its keyframe, ends its last picture at
  the cut (marked `SPLIT_HOLD`), carries a SegmentUID hashed from its range,
  holds no open-GOP leading picture, and is cut on the first video track; part
  names and chapter splits differ (see Fixed).
- A Matroska join of unlinked files drops a later file's priming and writes a
  negative `DiscardPadding`.
- A Matroska rewrite no longer writes colour or display size the reader took
  from the codec, writes a language as its ISO 639-2 code plus BCP-47 tag, and
  numbers an added track after every track with a free TrackUID.
- `to-mp4` sets the alternate groups; MP4 chapters end with the media; an ABR
  master's `BANDWIDTH` counts the shared audio; a CENC Matroska plan offers no
  I-frame playlist; fragmented MP4 decode times are strictly increasing.
- The probe reports another frame rate for a variable-frame-rate video, a
  MOV's QuickTime title and the chroma siting; `retime` and `mp4.Diagnose`
  number MP4 tracks as the probe does; a time bound like `1.001` is 1001 ms.
- `Validate` and `Diagnose` report damage on files with junk 0.55.1 passed
  over without a word (`Container.ResyncedBytes`), and `mp4.Diagnose` a
  `sample-table` finding.
- `go get ...@latest` skips the retracted versions v0.13.0 to v0.55.1.

### Changed

- The WebAssembly module grows by 394 KB: 9.52 MB at v0.55.1, 9.91 MB here
  (built with `-s -w` by Go 1.27.3).
- The probe reports a video track's chroma siting
  (`colour_layout.chroma_siting_horz` and `chroma_siting_vert`) when the
  Colour element states it.
- In WebVTT output, a CRLF or CR line end in a cue's text becomes LF (SRT
  written on Windows, typically): the `.vtt` bytes change, its text and timing
  do not, and all three are WebVTT line ends.

### Security

Encryption:

- No two samples of a track share a 'cenc' IV: decode times are strictly
  increasing, and a repeated IV or a decode time past 2^48 ticks is refused,
  across an on-demand plan's segment boundaries too, where a sample timed
  into the next segment no longer lasts zero or negative ticks.
- 'cenc' refuses two tracks whose IVs coincide (IDs equal modulo 255), an ABR
  ladder past 256 variants, and a 16-byte IV leaving its block counter no room.
- Concatenated presentations refuse CENC: every part restarted the same IVs
  under one key.
- Versions v0.13.0 to v0.55.1, and the pseudo-versions after v0.12.0, are
  retracted for the flaws above and below; `@latest` skips them.

Remote sources:

- Errors never show a URL's query or `user:password@`, `Stat().Name()` and
  the mux, merge and compare errors included.
- `httpfs` refuses an https to http redirect while `Options.Header` is set.

Crafted files (each was an allocation or a crash a few bytes could trigger):

- Manifests escape a file's strings: HLS drops `"`, CR and LF, DASH
  attributes are XML-escaped (a title could add a `BaseURL` to another host).
- Manifests escape every URI, `RewriteURL`'s, `KeyURI` and CMAF file names:
  HLS percent-encodes `"`, CR and LF and writes a backslash or a control
  character as it is instead of a Go escape; DASH XML-escapes them, so a
  signed URL's `&` is now written `&amp;`.
- An MP4 declares at most 16M samples in all (a PCM track counts its packets,
  not its one-frame samples), and a segment holds at most 256 MiB of media.
- `LoadAttachmentData` grows its buffer as bytes arrive instead of trusting
  the declared size.
- Empty CuePoints are skipped and Cues past 8M points are refused as damage.
- A PGS decoder bounds the objects one epoch holds together to 16 MiB: a stream
  that never starts a new epoch could pile them up.
- `Fingerprint` of an MP4 remuxes through a temp file on local disk, as
  documented, instead of holding the source and its Matroska in memory (WASM
  keeps the in-memory path).
- A box read past 8 MiB grows its buffer to its own size, not the next power
  of two.
- `s3fs` refuses a key segment `.` or `..` before any request, so no server or
  proxy resolving the URL path can step out of the bucket.
- `EditInPlace` and `ReindexInPlace` bound the SeekHead they read (a Seek
  declaring 2^56 bytes panicked), and `EditInPlace` refuses a head whose
  declared sizes run over the clusters instead of zeroing them with its Void.
- `serve --direct` mounts a file whose name holds a space or a brace, where the
  route panicked.
- A fragment sample declared past the source is refused before its buffer is
  allocated.
- An attachment left on disk (`WithoutAttachmentData`) and written with
  nothing to read it is refused instead of panicking.
- CENC: a picture of more NAL units than a saiz entry can size has its
  least-protected subsamples left clear, so saiz states each senc entry exactly
  (it was truncated past 40); an AV1 frame of several tiles is refused, not
  encrypted as one tile.
- MP4: a probe, subtitle or first-frame sample declared past its bound (64 KiB,
  1 MiB, 64 MiB) is refused before reading.
- Watermark: a negative segment number no longer panics; refusals show the
  source path without credentials.
- A forged CueTime no longer yields negative keyframes.
- CMAF: a representation ID is refused unless it is a plain name (letters,
  digits, `-`, `_`, `.`) other than `master`, since it is written in the MPD
  and names its playlist.

### Added

- `HLSPlan.Stat`, `ConcatPlan.Stat` and `ABRPlan.Stat` say what `Open` would
  hand out (size, ETag, content type) without handing it out; `mkvhttp`
  answers a HEAD and a matching revalidation from them, so neither counts a
  segment served nor takes one the walk kept.
- `ABRPlan.Open`: a variant's segment streams from its source through
  `mkvhttp` as an `HLSPlan`'s does, where it was built whole in memory.
- `DemuxOptions.NoOverwrite` refuses before writing when an output name is
  taken; `demux` sets it unless `-f`.
- `TrackType` names the complex, logo, buttons, control and metadata types
  (`TrackTypeOf`, `Number`), so a rewrite carries them; `CompressionUnknown`
  marks a `ContentCompAlgo` Matroska does not define.
- `mp4.ErrNoResource`: every plan's error for a name it does not serve matches
  it.
- `mp4.OpensLikeISOBMFF` reports whether a file's first 8 bytes open an
  MP4/MOV; `Track.Derived` holds what the Matroska reader took from the codec
  bitstream where the container stated nothing, so a rewrite writes only what
  was stated.
- `matroska.ErrNoRoomInPlace`: `EditInPlace` refusals for want of room match
  it with `errors.Is`, message unchanged, the file left as it was.
- `ExtractSubtitleWebVTT` on Matroska reads a track the source's Cues fully
  index (verified against `NUMBER_OF_FRAMES`) where the Cues put it, instead of
  walking the file, and walks otherwise with the same output. A source that
  stops before its declared end writes the cues it holds and returns
  `ErrSourceTruncated`.
- The WebAssembly `extractSubtitleVTT` takes a `Blob`/`File` too, read by
  ranges like the other Blob-backed exports, instead of a whole `Uint8Array`;
  on a Matroska source cut before its declared end it rejects with the WebVTT
  of the cues it holds in the error's `partial`.
- Native MP4/MOV editing: `mp4.EditInPlace`, `EditMetadata`, `SetChapters`,
  `RemoveTrack`, `Rewrite`, `Split`, `Join`, `Mux`, `Merge`, `AddTrack` and
  `MergeSubtitle` copy samples byte for byte and rewrite only the tables, and
  the CLI routes an MP4/MOV to them (`edit*`, `set-chapters`, `remove-track`,
  `add-track`, `mux`, `merge`, `merge-subtitle`, `split`, `join`; `to-mp4`
  rewrites it fast start). An in-place edit rewrites the moov alone,
  crash-ordered like `retime`. Split parts are chained by `org.mkvgo` freeform
  tags (`SPLIT_UID`, `SPLIT_PREV`, `SPLIT_CUT`) that a join reads and does not
  write; a join holds an audio gap up to 500 ms and refuses a longer one. An
  output of a hashed MP4 (`to-mp4 --hash`) hashes its own tracks, so `verify`
  passes on split parts. Fragmented and encrypted MP4, an output that is one of
  the inputs, and a mux, merge or join mixing MP4 and Matroska are refused.
- `mp4.LanguageFields` turns a user's language into the 3-letter code and the
  BCP-47 tag; `mkvgo.EditInPlace` routes either container.
- `subtitle.ParseSRTReader` (and `matroska.ParseSRTReader`) parses SRT from a reader.
- `split` part names never collide (case aside), chapter titles are cut to
  200 bytes and avoid Windows device names, and a bad pattern is refused first.
- `mp4.Diagnose` reports a `sample-table` finding: a table the file cannot
  hold, or a sample outside every `mdat`.

### Fixed

Split and join:

- A Matroska split opens every part on its keyframe at 0. The sound stored
  after the cut keyframe but timed before it goes to the part before. Another
  video track's pictures stay in the part before up to that track's own
  keyframe. On an open GOP the leading pictures, which decode only with the GOP
  before, stay out of both parts. A file is cut on its first video track only.
- A Matroska split by chapters starts the first part at 0, puts the gaps
  between stated chapter ends and what follows the last chapter in a part, cuts
  chapters in time order, and keeps a chapter starting after the last video
  keyframe in the part before. A chapter holding no keyframe is named, with a
  split by time offered.
- Split refuses, by name and before writing any part: a range that ends before
  it starts or starts past the end, a Matroska source that stops before its
  declared end (`ErrSourceTruncated`), and a part that would be named like its
  source.
- Split then join gives a Matroska source back sample for sample, second video
  track and cuts between milliseconds (29.97 fps) included, an open GOP's
  leading pictures aside: Matroska has no edit list, so they are in neither
  part (see the cut policy in cli.md). Linked parts are placed at the cut the
  split recorded, sound muxed far ahead included, so no seam drifts by a
  millisecond or by the sound's lead.
  A subtitle cue on screen across a cut opens the next part for the time it has
  left and is given back once, without holding the timeline. A chapter cut
  across a seam is whole again, its sub-chapters and end included, only from
  the part it continues, and a chapter reusing an earlier one's UID is kept. A Matroska part's SegmentUID
  hashes its range, so parts of two splits cut elsewhere never chain.
- A Matroska join of files that merely follow each other plays no later
  file's encoder priming: its whole priming frames are dropped and the rest
  inside the next frame (an Opus pre-skip) is marked as a negative
  `DiscardPadding`.
- A Matroska join refuses tracks that do not pair: another compression,
  encryption, picture size, sample rate, channel count, bit depth or language
  (`und` pairs with any, an audio field left out joins its stated default).

Subtitles:

- A cue that states no duration leaves the screen once read (17 characters a
  second, between 1 and 7 s), at the next cue that starts after it or at the
  film's end, whichever comes first, by one rule in SRT, ASS and WebVTT
  extraction and in a remux to MP4; HLS applies it too, the last segment
  bounding the last cue instead of the film's end (`subtitle.SettleCueEnds`,
  `CueEndMs`, `ReadingMs`). SRT and ASS
  extraction gave every cue 3 s, over the next one, and other paths kept a cue
  up to the next however far it was. A cue with a BlockDuration ends there.
- SRT extraction refuses a bitmap track, naming `-format pgs`. `MergeASS` keeps
  each event's layer, and `MergeASS` and `merge-subtitle` mux events in time
  order. An SRT timestamp with a dot before its milliseconds is read and a
  negative one refused; a blank line or an arrow in a WebVTT cue no longer ends
  or times it; an MP4 tx3g `<` that no tag closes is kept.

Chapters:

- A remux to MP4 writes one chapter list, in `chpl` and in the QuickTime
  chapter track alike, ending with the media: `chpl` kept chapters
  past the end, the last chapter ran 2 s when the duration was unknown, and the
  chapter track could make the movie outlast its media.
- An MP4 `chpl` listed out of order is read in time order, each chapter ending
  at the next.

Matroska writing:

- A rewrite (`EditInPlace`, `EditMetadata`, `Split`, `Join`, `Mux`...) keeps
  what it read and has no field for: the TrackEntry, Video and Audio children
  (`Track.Unmodeled`: FlagLacing, the accessibility flags, StereoMode,
  DisplayUnit, FrameRate...), the Colour sample layout (`Track.ColourLayout`)
  and every block's `DiscardPadding` (`Block.DiscardPaddingNs`). It no longer
  writes the colour and display size the reader took from the codec where the
  container stated none.
- A language set on a track (edit, mux, add-track, an `EditMetadata` callback)
  is the one in effect, written as its ISO 639-2 code plus its BCP-47 tag
  (`pt-BR` becomes `por` + `pt-BR`); one passed through unchanged keeps its tag.
- `EditInPlace` copies the elements an edit leaves as they were from the file,
  leaves Tags, Chapters and Attachments after the clusters there (rewriting
  them there when the head cannot hold them), finds them without a SeekHead,
  measures the head with its size fields as wide as the file writes them, and
  bounds the SeekHead it reads; `ReindexInPlace` refuses bytes past the
  Segment's end. Title edits were refused for want of room, chapters read
  twice, or the head left broken for readers that do not resync.
- No writer takes one of its inputs as output: every Matroska op, the MP4
  remuxes, the HLS, ABR and concat packagers (a source in the output directory
  under a name they write), the files lazy attachments are copied from, and
  `-f` on every command. They truncated the file they read, then removed it.
- `add-track`, `merge-subtitle` and `MergeASS` number the new track after every
  track with a free TrackUID. `--replace` flushes the verified copy before
  renaming it; `--keep-backup` refuses to overwrite an existing `.bak`.

Matroska reading:

- `ReadStream` describes a file as `Read` does, through the same decoders (held
  together by `FuzzReadVsReadStream`); its BlockReader times laced frames by their DefaultDuration
  and drops blocks of undeclared tracks.
- A file cut between two elements of a Cluster of known size is read as
  truncated, as a cut inside a block was. Junk between the segment's elements is
  resynced past.
- `ContentEncodingScope` is read: a compressed CodecPrivate is inflated, and a
  compression scoped to it leaves the blocks plain.
- Duplicate Info or Tracks keep the first; an unknown-size Tracks no longer
  hides the next one. A child overrunning its parent, or a block its Cluster, is
  bounded or reported as damage. Empty elements take their schema default,
  strings end at their first null, and values past their field's width are
  left unknown; an empty DisplayWidth or DisplayHeight means square pixels; a
  cue point without a time or position is dropped; a forged CueTime yields no
  negative keyframe.
- The lace decoder refuses sizes that are not sizes and reads a one-frame lace
  as that frame; a Cluster Timestamp that would wrap a block's time is damage.
- Every kept entry is charged to the metadata budget, and statistics tags find
  their tracks by UID. A PGS object referenced many times is decoded once per
  display set, and a display set whose objects lie more than 4096 pixels apart
  is refused as damage.

Damage and resync:

- The keyframe walks resync past a block that overruns its Cluster, and both
  readers resync only on an element that fits the Segment. Damage ahead of the
  first Cluster, or running to the end of the file, is reported once through
  `OnSkip`.
- `Container.ResyncedBytes` counts the junk a read passed over even when
  nothing recognizable follows it, up to the Segment's end (`ReadMeta` up to the
  window it scans): such a file no
  longer reads as sound, and `Validate` and `Diagnose` name `reindex --resync`.
- A subtitle index pointing at bytes no Cluster holds is stale, not damage; a
  cue without a duration ends where a damaged region begins.
- A negative `DamageBytes` refuses any damage in `mp4.Options` and the CLI too;
  `reader.DamageError` names the repair in library terms. A stream reader reads
  a large skip through, and `ReadStream`'s walk ends at a known Segment's end.

MP4:

- `to-mp4` groups alternative tracks as players expect (audio 1, subtitles 3, the chapter track none):
  Apple players showed one audio language.
- An MP4 cut short (its moov at the head) remuxes and serves what it holds. A
  track switching sample descriptions partway is refused by name for a remux
  or HLS, which carry one.
- A variable-frame-rate video reports the rate most of its frames play at, in
  the probe and in the HLS `FRAME-RATE`.
- `retime` and `mp4.Diagnose` number tracks as the probe does, and `retime`
  moves only the empty edit ahead of a track's media, saying how far to delay
  the others when a shift is refused.
- Tick conversions saturate; a MOV opening on `pnot` or `uuid` is recognised;
  sync tables, `trun`, `elst` and `sidx` are checked as read, a chapter sample
  past 64 KiB is ignored and containers nest at most eight levels deep. A MOV's
  QuickTime user-data title is read, and the `edit` JSON patch refuses unknown
  fields and tracks and accepts `language_bcp47`.

HLS and serving:

- A plan serves a WebVTT segment at the cost of its window, not of every cue
  before it: a player walking a long subtitle track no longer pays for the
  whole track on each segment.
- `GrowingHLSPlan` serves its requests side by side and while `Refresh` reads
  new clusters, its lock taken by a scan only to publish; `Complete` closes the
  playlist on what was published when the tail cannot be read.
- `HLSPlanStats.StreamedBytes` counts what an `Open` handle sends (a range its
  part, a cut write what it wrote, a HEAD or a 304 nothing); `ResourceHandle.Whole`
  tells a held resource without counting it. An MP4 plan counts `BuiltBytes`
  and `ServedBytes`.
- An ABR master counts the shared audio in every video-only variant's
  `BANDWIDTH`. The HLS plan of an MP4 source settles an unconfirmed audio
  stride as the full pass does, so both serve the same bytes. A CENC Matroska
  plan offers no I-frame playlist.
- A name a plan (HLS, growing, concat, ABR) does not serve answers 404, not
  502, and `mkvhttp` finds no satisfiable Range on an empty resource.
- The window budget a walk kept for a viewer who never came is reclaimed by a
  later walk; a grid-timed audio segment built cold keeps its bytes when built
  again; a window built twice gives back what the extra copy reserved; a
  subtitle read that failed once is no longer remembered for the plan's life.
- `serve` and `serve-growing` time out slow clients (10 s header, 2 min idle,
  5 min HLS response). web: `useHLSPlayer` (React, Vue) keeps at most 30 s
  buffered ahead, removes what played, and fails on a SourceBuffer error.
- `httpfs` `ReadAt` asks again for the rest of a short read and refuses Rename;
  `s3fs` takes an endpoint with a trailing slash and names the region to set
  instead of HTTP 301.

Other:

- A time bound like `1.001` is rounded to 1001 ms, not truncated; the AC-3,
  E-AC-3 and Opus sample entry refusals and retime's header refusals wrap
  their cause.
- WebAssembly: a value of the wrong type (a string for a segment index, a key
  as an ArrayBuffer, a `signal` that is not an AbortSignal) is a rejected
  promise naming the argument; it panicked and ended the Go program.

### Documentation

- `SECURITY.md` says how to report a vulnerability and what is in scope.
- `docs/library.md` says what a HEAD, a 304 or a short Range costs a source,
  what a server's declared sizes can make one open allocate, and that only
  `Diagnose` looks for a sample outside every `mdat`.

## Older versions

Versions before 0.56.0 are archived in [changelog/](changelog/): [0.50-0.55](changelog/0.50-0.55.md), [0.40-0.49](changelog/0.40-0.49.md), [0.30-0.39](changelog/0.30-0.39.md), [0.20-0.29](changelog/0.20-0.29.md), [0.10-0.19](changelog/0.10-0.19.md), [0.3-0.9](changelog/0.3-0.9.md).
