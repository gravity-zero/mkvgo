# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/).

## [0.27.0] - 2026-09-01

**Highlights**

- **A seek index is judged against the picture, not the container.** The
  stretch from the last video cue to the declared duration is, on real files,
  sound outlasting picture - not a hole - and files cued every 2 s were being
  condemned for it. The tail is now measured to the video track's own end,
  from its statistics when they describe the file, else from a bounded walk.
- **A sparse index is probed where it is sparse.** Each hole's clusters are
  walked header-only and the remedy follows what they hold: a reindex where
  keyframes went uncued, the source where the picture is missing, a re-encode
  where a stretch has no keyframe - instead of a repair loop that never
  converges.
- **Where each track really ends.** `TrackEnds` measures content against
  content; `Diagnose` raises `audio-short` and `picture-missing` from it, and
  states the file's real, declared and missing sizes on both engines.
- **No op that merely carries an attachment loads it.**

### Added

- **Where each track's content really ends.** The declared duration is the
  longest track's end - on real files an audio track's - and says nothing
  about the others: an audio track that dies minutes before the picture leaves
  a structurally healthy file whose playlists promise audio that can never
  exist. `TrackEnds` (`track-ends`, WASM `trackEnds`) measures content against
  content: the per-track statistics `DURATION` tag when it describes this file
  (same writing application and date, not past the declared duration - the
  checks a muxer applies before believing one), else a bounded header-only
  walk of the tail from a cue a window before the end, widened once for a deep
  gap; a track silent through the widest window is reported as ending at or
  before it rather than guessed. `Diagnose` raises `audio-short` from it, and
  `CueHealth` measures its tail against the same trusted picture end.
- **A missing stretch of picture is a finding of its own.** The episode whose
  cues matched its keyframes one for one freezes for 50 and 61 s: a playback
  defect whatever the index does. `Diagnose` now raises `picture-missing`,
  located, from what the hole probe saw - next to the index verdict, not
  folded into it - and, for a file that states no statistics, hands the
  picture end its tail walk established back to the index verdict, so the
  tail of an untagged file is no longer measured against an audio track's end.
- **A sparse index is probed where it is sparse, and the remedy follows.**
  Three files with the same 60 s hole in their video cues want three different
  answers - keyframes nobody cued (a reindex closes it), frames without a
  keyframe (only a re-encode makes the stretch seekable), a stretch with no video
  at all (the picture is missing from the stream; nothing can cue it) - and sent to a
  reindex regardless, the last two come back exactly as sparse: a repair loop
  that never converges. `CueHealthReport` now lists its `Holes`, and
  `ProbeCueHoles` walks the clusters between the two cues around each one -
  block headers alone, payloads skipped, constant memory, nowhere else in the
  file - and pronounces what it holds; it refuses to guess on a cue whose
  position is not a cluster or a walk that never reached the far side.
  `Diagnose` runs it for every `index-sparse` finding and restates detail and
  remedy from what it found; `cue-health -probe` prints the same. Measured on
  the episode above: three holes, 28 to 61 s without a video block in each,
  "re-acquire the source" -
  from the index alone, no statistics tag needed.
- **`analyze` reports the widest keyframe gap in time, and where it opens.**
  The GOP statistics count frames between keyframes, so a stretch the frames
  are missing from is invisible to them: the episode above read "GOP max 253
  frames, keyframe every ~4.6 s" across a 61 s hole, because only 253 frames
  stood between those keyframes. `MaxKeyframeGapMs`/`MaxKeyframeGapAtMs` put
  the hole on the clock next to the cue verdict that names it.

### Changed

- **The facade reads everything head-only that the reader can.** `WithTags`,
  `WithChapters`, `WithAttachments`, `WithoutAttachmentData`, `WithCues` and
  `WithoutTailScan` are re-exported on `matroska`, so a caller never has to
  reach into `mkv/reader` for a metadata read.
- **A diagnosis states the file's sizes.** `Diagnosis.FileSize`,
  `DeclaredSize` and `MissingTailBytes` (both engines) put a number on a
  truncated file - the magnitude that routes the operator's decision, which
  callers were re-parsing the container header to obtain.
- **No op that merely carries an attachment loads it.** Removing or adding a
  track, editing metadata, splitting, merging, remuxing to WebM, and every
  read-only pass (validate, compare, analyze) now open their source with the
  attachment payloads left on disk, and the writers copy them through from
  there - as `Join` already did. A font set travels byte for byte without a
  byte of it resident; extracting an attachment still reads it whole. An
  `EditMetadata` callback therefore sees `Attachment.Data` nil on the
  attachments it inherits (`DataPath`/`DataOffset`/`Size` say where they are)
  and writes the ones it adds from `Data` as before. **Behaviour change,
  same signatures**: a callback that read `Data` on an inherited attachment
  now finds it nil - call `LoadAttachmentData(ctx, &att)` (new, facade and
  `ops`) to fill it from disk; nothing in mkvgo's own callers did.

### Fixed

- **A file's seek index is judged against the picture, not the container.**
  The worst hole in the video cue coverage counted the stretch from the last
  video cue to the declared duration - which is the longest track's end, on
  real files an audio track's, 30 to 110 s past the last frame. Files cued
  every 1 to 10 s were condemned as `index-sparse` for their end credits, sent
  to a reindex that changed nothing, then to a re-encode: measured on a library
  scan, four such verdicts in five. The tail is now measured to the video
  track's own end, read from the statistics `DURATION` tag mainstream muxers
  write per track once the tag is shown to describe this file (same writing
  application and date, not past the declared duration - what a muxer checks
  before trusting one); without it the tail counts only past what an
  outlasting track accounts for. The same four files: one GOP past the last
  cue, healthy. A dense index that stops while the picture goes on stays
  sparse either way.
- **A hole the picture itself is missing from is not an index defect.** The
  fifth verdict was an episode whose 771 cues matched its 771 keyframes one for
  one: its 50 to 65 s holes were frames absent from the video track, and a
  reindex - however faithful - left them where they were. The track's own
  statistics say so head-only (`NUMBER_OF_FRAMES` short of the duration at the
  frame rate, 183 s on that file); when they account for the hole the reason
  says the picture is missing there and the remedy is the source, not a
  reindex. `CueHealthReport` gains `MaxVideoGapAtMs`, `VideoEndMs`,
  `VideoEndExact`, `TailGapMs` and `VideoShortfallMs`; every reason names
  where the hole is ("the video cues stop at 00:00:59, 541s before the
  picture ends", "a 65s hole at 00:40:34"), and `cue-health` prints the
  picture's end and the tail. `mkv.ParseClockTime` is exported for the
  HH:MM:SS.fraction form the tag and OGM chapters share.

## [0.26.0] - 2026-08-03

**Highlights**

- **A split now round-trips.** Each part is a hard-linked segment - its own
  derived `SegmentUID`, chained through `PrevUID`/`NextUID` - and `Join`
  reads that chain: a seam between chained parts lands where the picture
  continues instead of after the latest measured track end. A 12-part film
  comes back 8 ms off end to end (from 909 ms), every chapter within a few
  milliseconds of the instant it was cut on, block for block identical.
  Appending unrelated files keeps the ordinary measured seam.
- **An output that differs from its source owns its identity.** Removing or
  adding a track, merging subtitles - each derives its own `SegmentUID`
  deterministically instead of claiming to be the file it came from; the WebM
  remux carries none at all, per the WebM subset; repairs keep the identity
  untouched.
- **Attachments never become resident.** A joined font set streams from the
  source file to the output without passing through memory - bytes unchanged,
  peak memory no longer scales with the attachment set. Matters wherever mkvgo
  runs in a small container.
- **Prove a split lost nothing without rebuilding the film.**
  `compare -blocks` (and `CompareBlocksConcat`) diffs one file against the
  concatenation of its parts, per track, byte for byte - 12 parts of a 2 GB
  film in under nine seconds, no temporary copy.

### Fixed

- **A rejoined film keeps the timeline it was cut from.** A cut runs down the
  file at a video keyframe, so it is exact on the video track and on no other:
  interleaving leaves the part before it holding sound from after the cut, and
  the part after it opening on sound from before it. Rejoined on the latest
  measured end, every seam gained that overlap - 83 ms a seam, 909 ms over the
  eleven seams of a 12-part film, and the picture dragged through every one of
  them. `Split` now chains its parts through their segment identity
  (`PrevUID`/`NextUID`), and a seam between chained parts lands where the
  picture continues: the same film comes back 8 ms off end to end, block for
  block identical. Files that merely follow each other - episodes, recorder
  chunks - are unaffected: their seam stays after everything the previous file
  holds, audio tail included, and so does the seam between parts joined out of
  order or with one missing.
- **A chapter marker stays on the picture it names.** A part is cut on the first
  keyframe at or after the bound it was asked for and keeps the GOP straddling
  its end, so a marker sitting between the two names a frame the PREVIOUS part
  holds - and, selected on the requested bound, it came to the next part anyway
  with nowhere to sit but zero. Every chapter of a rejoined split was announced
  up to a GOP early: measured on a 12-chapter film, 287 ms out on ten of them
  and 2.3 s and 3.1 s on the last two, now 1 to 7 ms. The selection is made
  against the CUT, which consecutive parts share, so each marker goes to the one
  part that holds its frame; a part still opens on the chapter that is playing
  when it starts, and a `Join` of linked parts knows that repeat is the same
  chapter still running rather than a second one.
- **Every part of a split is a segment of its own.** Each carried the source's
  `SegmentUID`, leaving twelve files all claiming to BE the file they came from.
  They now get their own, derived from the source so that splitting the same
  file twice still writes the same bytes. A joined file likewise stops wearing
  its first part's identity, whose `NextUID` sent a player looking for the
  second part - which is inside it. A source that is itself a slice of a larger
  timeline keeps its own links at the chain's ends: the first part still
  succeeds the source's predecessor, the last still precedes its successor.
- **A merged subtitle cue that outlasts the source extends the declared
  duration.** `MergeSubtitle` and `MergeASS` inject every cue, late ones
  included, but copied the source's authoritative `Duration` verbatim - the
  output played past the length it declared. Both now restate it to the last
  cue's end, exactly as `AddTrack` already did for a longer track; a cue that
  fits changes nothing, the source's declaration surviving untouched.
- **The same rule holds for every op whose output differs from its source.**
  `RemoveTrack`, `AddTrack`, `MergeSubtitle` and `MergeASS` all copied the
  source's `SegmentUID` onto a file with different content; each now derives
  its own, deterministically, per op. The hard links (`PrevUID`/`NextUID`)
  stay untouched - adding or removing a track does not move the timeline, so a
  subtitled part still joins back at the precise seam. `RemuxToWebM` carries no
  segment identity at all: the WebM element table lists `SegmentUID`, `PrevUID`
  and `NextUID` as Unsupported, and the source's used to be copied in anyway.
  Ops that repair or restate a file without touching its content (`Reindex`,
  `Salvage`, `RetimeTracks`, `EditMetadata`) keep the identity: same content,
  same segment.
- **A file declares the length it holds.** `Join` announced the duration of its
  first source, `Split` gave every part the whole film's, and `AddTrack` ignored
  a track that outlasted the file it was added to: the duration each op computed
  was silently overridden by the one copied from the source. An edit callback
  setting `Info.Duration` still wins, which is how a caller restates a length.
- **`Join` keeps the tracks of a file aligned with each other.** Each track used
  to be rebased on its own end, so a track that stopped earlier than the others -
  a subtitle track whose last cue is minutes before the end - came back that much
  early in the next file. On a real episode split at 10:00 that was 5m50s of
  desync; past ~32 s the block no longer fits a SimpleBlock's timecode and the
  join failed outright. One offset now shifts every track, taken from the end
  measured at the seam rather than the one declared.
- **`Join` keeps every source's chapters**, shifted onto the seams actually
  written, instead of only the first file's: splitting a film on its chapters and
  joining the parts back returns all of them. Colliding ChapterUIDs are
  renumbered, chapters linked to another segment are dropped.
- **A chapter with no explicit end runs until the next one starts**, so a part no
  longer inherits every chapter before it. That next chapter is the next in TIME,
  not the next in the list - editions arrive concatenated, so reading the list
  order made a chapter vanish from the slice it names and `Split` write empty
  parts without an error.
- **Nested chapters survive a write.** `WriteChapters` emitted only the top level,
  so sub-chapters were dropped by any op that rewrote a file.
- **`Split` and `Join` measure the tags that describe the media** - the content
  hash and the per-track statistics - instead of copying the source's. Every file
  they produced failed mkvgo's own `VerifyContentHashes`, and a ten-minute part
  reported the whole film's bitrate and frame count. The statistics are now always
  attached; the content hash follows the source's intent.
- **`Join` decodes a source's timecodes with its own `TimecodeScale` and writes
  them with the output's.** One scale served both jobs, so appending a file muxed
  at a different scale wrote ticks against a divisor the header does not declare -
  the media came out stretched, silently.
- **Attachments are pooled from every joined source**, identified by content: a
  font attached only to the part that uses it is no longer lost, one repeated in
  every part lands once, and cover art stays single.
- `Join` no longer fails on sources that declare no duration at all, which left a
  truncated file behind.
- **Attachments never become resident.** `reader.WithoutAttachmentData` records
  where a payload lives instead of loading it, and the writer copies it straight
  from the source file, so joining files with a large font set no longer scales
  memory with that set - it used to peak at four times its size. The bytes
  written are unchanged. Matters wherever mkvgo runs in a small container.
- A chapter is clipped to what its source actually wrote, so one sitting in the
  phantom tail of a file that declares more than it holds no longer lands on the
  next source's frames.
- The per-track statistics describe every declared track, including one that
  received no block (zero frames) or a single frame (no measurable duration);
  both used to lose their statistics entirely. They now also carry the
  conventional `_STATISTICS_TAGS` / `_STATISTICS_WRITING_APP` markers, which were
  stripped and never restated. No date is stamped, so two identical runs still
  produce identical files.

### Added

- `compare -blocks` accepts several files on the right and diffs the left one
  against their **concatenation** (`matroska.CompareBlocksConcat`), proving a
  split kept everything without rebuilding the joined file - 12 parts of a 2 GB
  film in under nine seconds, no temporary copy.

## [0.25.1] - 2026-07-18

### Fixed

- **Constant-rate B-frame video was classified `vfr`.** `Analyze` measured
  frame durations as the delta between consecutive stored blocks - but
  Matroska stores blocks in DECODE order carrying PRESENTATION timecodes, so
  on any B-frame stream (most modern video) the stored-order deltas jump
  around (+125, -84, +41, ...) even when every frame is presented on a
  perfectly constant cadence. On top of that the verdict keyed on the raw
  max-min delta spread, a statistic one single outlier dominates: a lone
  dropped-frame hole among 150000 constant deltas flipped the whole title to
  `vfr`. Measured on a real constant-rate 23.976fps release, 77% of
  stored-order deltas sat off the modal delta and the track reported
  variable frame rate - a caller keying a copy-vs-transcode decision on that
  verdict transcoded content that was copy-eligible. `FrameRateMode` now
  restores presentation order through a bounded 64-frame reorder window
  (H.264/HEVC cap reorder depth at 16) before measuring deltas, and the
  verdict is outlier-robust: only when more than 1% of the
  presentation-ordered deltas (and at least 2) sit farther than the +-1ms
  quantisation slack from the modal delta is the track reported `vfr` - a
  genuinely variable track still is, an isolated glitch or splice no longer
  is. That real release now reads `cfr` with a 1ms spread (the legitimate
  41/42ms alternation of a millisecond-quantised 24000/1001 cadence).
  `FrameDurationVarianceNs` keeps reporting the raw spread as a diagnostic,
  and the new `TrackStats.FrameDurationOutlierFrac`
  (`frame_duration_outlier_frac`) exposes the fraction the verdict
  thresholds on, so a consumer can apply its own cutoff.

## [0.25.0] - 2026-07-18

### Fixed

- **A handful of surplus bytes made a 2 GB file unrepairable.** Some batch
  tools leave a few zero bytes PAST the declared Segment end - bytes no
  reader ever sees, since readers stop at the declared end - yet the rewrite
  engines (`Reindex`, `ReindexReplace`, `RetimeTracksReplace`, and the
  automatic `RetimeTracks` once it routes to the rewrite) walked the source
  to EOF, read the padding as a top-level element, and aborted the whole
  operation on `invalid VINT: leading zero byte` - after reading 100% of the
  file, and with a refusal message suggesting `Options.Resync`, which the
  retime path did not honor. The walk is now bounded by the declared Segment
  end: bytes past it that neither parse as an element nor carry any trace of
  a Cluster are dropped from the output and reported through
  `Options.OnSkip` (the CLI prints `dropped N B of trailing junk...`). Junk
  must prove itself first - a bounded scan reaching EOF without finding even
  the 4-byte Cluster ID - so real media behind an undershooting declared
  size keeps being copied, and a cluster cut mid-write, which no structural
  validation can see precisely because it is incomplete, keeps the strict
  refusal rather than being dropped with the junk around it. Corruption
  inside the declared Segment still refuses; that is what `Options.Resync`
  is for. The rollback delta carries the dropped bytes:
  `ApplyRollback` keeps reconstructing the original byte for byte, junk
  included. Verified on real releases carrying 4-33 bytes of zero padding:
  every repair that refused now lands, deep-verify and all, and a clean
  source still rewrites byte-identical.

- **Surplus bytes are not missing bytes.** The tolerant walk raised the
  `TruncatedTail` verdict - "incomplete download, re-download the source" -
  for ANY damage touching EOF, including bytes lying entirely past the
  declared Segment end. A caller relaying that verdict told its operator to
  re-download a file whose media is complete. `TruncatedTail` now requires
  the damage to begin INSIDE the declared Segment; surplus bytes still count
  in `DamagedRanges`/`BytesSkipped` and keep the `trailing-junk` finding, but
  never the truncated verdict or its remedy.

- **An OGM chapter whose hours field does not fit is refused, not wrapped.**
  The OGM simple-chapter format bounds minutes, seconds and the fraction but
  never the hours, so a time like `2700000000000:0:0` overflowed the signed
  64-bit millisecond it is converted to and wrapped into a large negative
  `StartMs` - a chapter apparently starting long before the file - instead of
  an error. `ParseOGMChapters` now rejects an hours field that would not fit;
  the largest time it accepts is `2562047788015:12:55.807`. Found by the fuzz
  suite; the crashing input is kept as a regression seed.

### Added

- **Typed retime refusals.** Every permanent refusal of
  `RetimeTracks`/`RetimeTracksInPlace`/`RetimeTracksReplace` now wraps an
  exported sentinel - `ErrShiftNotRepresentable`, `ErrShiftOutOfRange`,
  `ErrUnknownTrack`, `ErrTrackHasNoBlocks`, `ErrUnknownSizeSegment` - and the
  strict-walk refusals of the reindex/retime family wrap `ErrCorruptSource`
  (repair first, then retry). All re-exported by the `matroska` facade. A
  caller routes on `errors.Is` - "permanent for this call, do not retry"
  versus a transient failure - instead of parsing message text.

- **`wrong-container` finding.** `Diagnose` on a `.mkv` whose content is
  really ISO base media (MP4/MOV) used to return an error, putting the file
  back in the failed pile of every scan pass. It now returns one structured
  `wrong-container` finding (remedy: rename/remux, or route by content via
  the root `mkvgo.Diagnose`), settling the file once. `CueHealth` on the same
  file keeps failing but wraps `ErrNotMatroska` (`errors.Is`-able). Content
  that is neither container remains an error.

- **An optional audio frame converter for HLS packaging.**
  `Options.FrameConverter` re-encodes an audio track's frames from one codec
  to another as HLS segments are packaged - a way to serve a source codec a
  browser does not decode as one it does - through a small interface the
  caller implements, so the module itself stays dependency-free. When nil (the
  default) every frame is carried verbatim and the packaged output is
  byte-for-byte the unconverted presentation. The full pass (`RemuxToHLS`,
  `RemuxToABR`, from a Matroska or an MP4 source) honours it, feeding each
  track's frames through the converter in decode order; a claimed track is
  rebound to its output codec so the existing sample-entry machinery is
  reused. The on-demand plans refuse a converter for now - a shared converter
  cannot serve windows built out of order and in parallel; per-window
  conversion with preroll is a later step. See `docs/library.md`.

- **MPEG-TS is named.** The root sniff behind `RetimeTracks`/`Diagnose`
  recognises an MPEG-TS transport stream - a `0x47` sync byte confirmed by a
  second one exactly a packet (188 bytes) later - and reports it with remux
  guidance instead of the generic "unrecognised container" message. A lone
  `0x47` stays unrecognised.

## [0.24.0] - 2026-07-14

### Changed

- **A viewer read the source twice to be served once.** A direct-play server's
  ceiling is its storage bandwidth, so a byte read and thrown away is a viewer it
  cannot serve. A viewer does not pull video alone: it pulls the video segment AND
  the audio segment of the same instant, and those were two separate walks over the
  SAME window. Neither could skip the other's bytes - a source interleaves its
  tracks block by block, and in a real file every payload (a ~10 KiB video frame, a
  ~2 KiB audio frame) is far under the reader's seek-vs-read threshold, so a track
  filter skips none of them. Each walk therefore read 100% of the window to serve
  its own slice of it: the video ~77%, the audio ~11%. Read twice, serve once.

  The walk that reads a window now frames EVERY rendition of it - it had to read
  all of their bytes to reach its own - so the sibling segments of one instant are
  already built when they are asked for, and cost no read at all. Requests that
  race (hls.js pulls video and audio in parallel, and nothing orders them on the
  server) share the one walk rather than opening a second over the same bytes.

  A window is held only while it is in flight. A rendition's bytes are freed the
  instant they are delivered, and the window goes as soon as a viewer has what a
  viewer takes - its video and ONE audio track. Waiting for the rest would be
  waiting for a request that never comes: nobody fetches the second language or
  the subtitles, so a bundle held until exhaustion sits in the cache with its
  leftovers, and on a source with heavy audio those leftovers crowd out the windows
  that ARE about to be collected - the saving decays as the viewer watches on, and
  a seek loses it outright. Dropping on the consumption profile keeps the retained
  media at ~0.5 MiB per plan (it was 25-87 MiB when the leftovers stayed). A viewer
  who switches language re-walks one window; a miss is correct by construction.

  `Options.WindowCacheBytes` is what remains as a net - it bounds a client that
  takes a video and never an audio - and left at zero it is **derived from the
  source**: twice the largest window, floored at 32 MiB. A fixed ceiling is wrong
  for somebody by construction (a 1080p window runs ~2 MiB, a high-bitrate 2160p
  one ~22 MiB), and one under a window's size evicts it before the player has
  collected its audio, so the second request re-walks and the saving evaporates on
  precisely the biggest files. A miss - a seek, a cold plan, an evicted bundle, the
  cache turned off - simply walks the source as before, so serving stays stateless
  in effect: no request depends on another having happened.

  Measured on **34 real releases** (rchar, 100 windows plus a mid-film seek, the
  video + audio a viewer actually pulls): every one of them halves, **2.1-3.7x down
  to 1.07-1.84x**, 1080p and 2160p alike, the heaviest 2160p remux (22 MiB windows)
  landing at 1.08x, and the figure holds through a seek instead of decaying. The
  source is read once per viewer instead of twice. Every resource is byte-identical
  with the cache on, off, or evicted - checked against real sources, not only
  fixtures.

### Added

- `Options.WindowCacheBytes` bounds the media an on-demand plan holds for the
  renditions nobody asked for. Zero derives it from the source (twice the largest
  window, floor 32 MiB); negative disables the sharing entirely.

## [0.23.0] - 2026-07-14

### Changed

- **A segment walk can open on the window's own first block, not on its cluster's
  header.** The Cues index can only name a CLUSTER, so a walk that opens there
  reads whatever of the cluster precedes the window. Where a cluster is long enough
  to hold a whole segment, that prefix was re-read once per segment inside it; the
  walk now remembers the block each window opens on (it already stops on precisely
  that block - that is how it knows the previous window ended) and resumes there.

  **This was released on a premise that does not hold for real sources, and its
  effect on them is nil.** Real releases cluster every ~0.4-2s, so a 6s segment
  SPANS three to fourteen clusters - there is no cluster prefix to re-read, and
  measurement on real 1080p/2160p files confirms the amplification is unchanged to
  the byte. The claim that this addressed the 6.7x read amplification was wrong: it
  came from a fixture built to the assumption (10s clusters) instead of to the
  measured layout, so it confirmed what it encoded. The real cause - a viewer's
  renditions each re-reading the same window - is fixed in 0.24.0.

  What this release does deliver: sources whose muxer writes long clusters do skip
  the prefix, and the segment walk stopped copying the other tracks' payloads out
  of the reader (`KeepTracks`), cutting per-segment allocations ~19%. It never
  changed a byte of any segment.

### Added

- `reader.BlockPos` / `reader.NewBlockReaderFrom` / `BlockReader.Pos`: a walk can
  now be resumed at an exact block, mid-cluster, instead of only at a cluster
  header (`NewBlockReaderAt`). The position carries the enclosing cluster's
  timestamp and end, so a resumed walk stamps its blocks exactly as a full walk
  does without re-reading anything ahead of them.

## [0.22.1] - 2026-07-13

### Fixed

- **The picture played one to two frames after the sound, in every fragmented
  output of a source with B-frames.** Decode times are derived by sorting the
  presentation times, which leaves composition offsets that may be negative -
  legal in a `trun`/`ctts` version 1, and what mkvgo wrote. A FRAGMENTED file
  cannot honour them: its decode clock lives in the `tfdt`, which is unsigned, so
  a reader cannot answer a negative offset by pulling the decode clock back before
  zero (a progressive file can - its sample table has no such floor - which is why
  `to-mp4` was in sync through all of this). It compensates the only way left to
  it: by pushing every video PTS forward by the deepest negative offset. Audio,
  never reordered, stays where it is. Measured on a real 23.976fps source: the
  first frame presented at 42ms instead of 0, the whole picture that far behind
  the sound - audible, and above the ITU-R BT.1359 threshold for sound arriving
  early. The composition offsets are now shifted non-negative before they are
  written, and the init segment carries the edit list that takes exactly that shift
  back out, so the presentation starts where the source put it: first frame at 0,
  not a sample lost, audio and video on one timeline. The shift is measured from
  the opening frames of the video track (its reorder depth is an encoder constant
  the first GOP already spells out) - header-only in the on-demand plan, which
  never reads a sample byte to time its init, and over the same prefix in the full
  pass, so the two still produce byte-identical inits.

  Every fragmented producer is covered and verified against a decoder on a real
  B-frame source - the full pass, the on-demand plan, the growing (still-
  downloading) plan, the single-file byte-range renditions, and a plan built from
  an MP4 source: all now present their first frame at 0, with no sample lost, and
  the three that can be compared produce byte-identical inits. The `sidx` of a
  single-file rendition now declares its real earliest presentation time (the
  shift) rather than a zero no sample carries.

  **Integration note**: the init segment's bytes change for reordered sources (it
  gains an `edts`/`elst`). Callers caching init segments must invalidate them.
  Callers that were compensating this delay themselves - shifting a separately
  encoded audio rendition to match the late video - must STOP: the video no longer
  arrives late, and the compensation would now push the sound behind it.

## [0.22.0] - 2026-07-13

One bug class, found on a real library and then hunted through the code: **the
head-only path answered "absent" for data the file carries, and the verdicts
built on it judged the wrong thing.** A file could clear one of these and still
fail the next, so they are fixed together - the seek index is now found wherever
a muxer left it, and "is this index any good?" is decided by what actually serves
a seek.

### Added

- `CueHealthReport.MaxVideoGapMs` (`max_video_gap_ms`): the widest hole in the
  video cue coverage - the worst distance a seek can land from its target.
- `Finding.Kind` `"index-sparse"`: the video cues are keyed right but too far
  apart to seek into.
- `reader.WithoutTailScan()`: turn the EOF fallback off, for the caller that must
  prove an index is reachable from the head itself rather than merely obtain it
  (ReindexInPlace's verifier).

### Changed

- **`CueHealth`/`Diagnose` now judge the VIDEO cues, not the share of non-video
  ones.** A cue keyed on an audio track is inert: the keyframe index that serves
  seeking is built from the video-keyed cues and drops the rest. Yet any single
  non-video cue condemned a file, and real muxers routinely cue *every* track -
  so files whose video index is dense and perfectly seekable were reported
  `index-misskeyed` (measured: a film with 2112 video cues flagged over 10 stray
  audio cues; an episode with 3333 video cues, one every ~2s, flagged because
  91.7% of its 40219 cues were audio). The verdict is now the video index's own
  ability to serve a seek: `index-misskeyed` when NOT ONE cue keys on video (the
  defect the check was built for - every seek lands mid-GOP), and the new
  `index-sparse` when the video cues leave a hole wider than 30s. The hole is
  reported as `MaxVideoGapMs` (`max_video_gap_ms`), measured between consecutive
  video cues and from 0 to the first and from the last to the duration, so a
  half-indexed file is caught too. `NonVideoPct` is still reported - a high share
  is index bloat a reindex slims - but no longer condemns anything.
- **`validate`'s `cues-non-video` follows the same rule**: an error only when no
  video cue survives (the index cannot seek video), a warning when video cues
  are there and the non-video ones are mere bloat.

### Fixed

- **The head-only path called a healthy seek index missing on every file with
  no SeekHead.** `reader.WithCues()` looked for the Cues only where a SeekHead
  pointed at them, so a file whose Cues sit intact at the tail - the layout most
  muxers produce, and the one the full reader has always handled with a bounded
  read back from EOF - came back with zero cues. Every head-only consumer
  inherited the false verdict: `CueHealth` reported `Healthy=false` with "no seek
  index: run mkvgo reindex", `Diagnose` raised a `no-index` finding, `Ingest` set
  `HasSeekIndex=false` and scheduled a reindex, and `PlanHLS` fell back to
  synthesizing an index the file already had. The metadata path now runs the same
  bounded tail scan as the full reader when nothing indexes the Cues (no SeekHead,
  or a stale Cues pointer that gets rejected), so both read paths reach the same
  verdict on the same file. A file that genuinely has no index still reports none.
- **Same hole, same trailing region, for the other head-only options.**
  `WithTags`/`WithBitrate`, `WithChapters` and `WithAttachments` also followed a
  SeekHead pointer and nothing else, so on a file that indexes none of its tail
  they returned nil while the elements sat right there behind the Cues - a
  per-track BPS bitrate (`Track.Bitrate`) reported as absent on the majority of a
  real library. The tail scan now hands over whatever the caller asked for. It
  runs only when something REQUESTED is still missing, so a plain metadata probe
  pays nothing and a file whose SeekHead delivered does not pay twice.
- **The metadata path was blind to everything written between Tracks and the
  first Cluster.** It stopped the moment it had Info and Tracks, and had no case
  for Chapters or Attachments on the way past (and kept Tags only for
  `WithBitrate`), so an element sitting there - a perfectly ordinary place to
  write one - was neither read in passing nor reachable afterwards: no SeekHead
  points at it and it is nowhere near EOF. It now reads what the caller asked for
  wherever it sits in the head, and stops early only when nothing requested is
  still outstanding. The scan still ends at the first Cluster: the media is never
  walked.
- **`ReindexInPlace` refused layouts it can actually repair.** With no SeekHead
  but a Void slot in the head, the patched index lands before the first Cluster -
  where a head-only reader walks straight past it. The verify called that "not
  head-discoverable" and rolled the file back, because the reader it asked
  stopped at Info+Tracks and never looked: a blind spot reported as a defect of
  the file. Those files are now patched in place instead of paying for a full
  copy rewrite. `ErrIndexNotHeadDiscoverable` keeps its meaning and now also
  wraps the two layout refusals that always did mean it (no SeekHead and no Void
  to patch into), so `Ingest` returns its plan with the copy-reindex fallback
  there instead of failing outright. The verifier asks for the strict answer via
  the new `reader.WithoutTailScan()`, so it tests what it claims - the head, with
  no EOF fallback - rather than accepting an index only a tail scan can find.
- **`Ingest` called an audio-keyed index a seek index.** `HasSeekIndex` was
  `len(Cues) > 0` - any cue, any track - while the consumer it speaks for
  (`PlanHLS`) cuts its segments on the VIDEO track's cues and refuses a source
  that indexes no video keyframe. A file whose index only cues the audio was
  waved through as "ready for on-demand HLS" and blew up at serving time, on the
  plan Ingest had just blessed. It now requires a cue the video can seek to.

## [0.21.1] - 2026-07-13

A correctness release: every fix below closes a path that **reported success
while leaving the file wrong** - the guard checked one thing and the write did
another, so nothing ever surfaced. No API change.

### Fixed

- **`EditInPlace` destroyed the seek index it was supposed to preserve.** The
  head SeekHead sits at the start of the metadata region, so every in-place
  edit writes over it; it was only rebuilt when the metadata still left room
  for the fixed 256-byte reserve, and below that the metadata was simply
  written across it - `nil` returned, Cues no longer reachable head-only. The
  SeekHead is now mandatory whenever the file had one: its slot is sized to
  fit exactly (the entry positions depend on the slot size, so the two are
  solved by iteration), and an edit that cannot host both the metadata and a
  SeekHead is refused with the file untouched, instead of trading the index
  for the edit. A second index-loss path went with it: the preserved entries
  (the Cues pointer among them) were read from the region start rather than
  the SeekHead's own offset, and were silently dropped whenever a Void
  preceded it.
- **`WriteVoid` could overrun its budget by one byte.** A Void padding a
  reserved slot must span EXACTLY the bytes it is given; the size-VINT width
  was guessed from an estimated payload, and at 129, 16386 and 2097155 bytes
  the element came out one byte longer - swallowing the head of the element
  that follows. The width is now widened until the span lands exactly (EBML
  allows a non-minimal Data Size). `ops.voidHeaderBytes` carried a private
  copy of the same arithmetic - and, writing only the header, made the
  DECLARED size the whole contract: a stale Cues span of exactly those sizes
  had `ReindexInPlace` void one byte past it, break the top-level element
  chain, and commit the result through its journal as a success. Both now go
  through one implementation (`writer.VoidHeader`). `EditInPlace` also filled
  a 1-byte leftover with a raw `0x00` - a byte no EBML element can be, where
  a parser expects one to start; the odd byte is now absorbed into the
  metadata (the first element's data size is re-encoded one byte wider).
- **`mp4.RetimeTracks` destroyed two file layouts it should have refused.**
  The repair lands its moov by appending at the end of the file: a last box
  declaring `size 0` (it runs to the end of the file - what streaming writers
  emit) simply grew over the appended moov while the old one was retired, and
  junk past the last box left the appended moov behind it, desyncing the box
  chain. Both are refused now - the shapes `mp4.Diagnose` already reports -
  with the file untouched. Third fix, same function: the scan took the LAST
  moov while a forward walk (and `mp4.Open`) reads the FIRST, so re-running
  the repair after an interrupted one retimed the stranded moov and left the
  live one alone - the shift silently lost, two live moovs on disk. The scan
  now targets the first moov and retires every stray one, tail first, so the
  file carries exactly one live movie header at every instant.
- **Checksums and position hints went stale under in-place patches and
  relocations.** `retime` patched the CueTimes under a Cues CRC-32 without
  recomputing it (the cluster CRC always was), same for a CRC-32 carried by a
  BlockGroup whose Block moved. `Reindex` and `Salvage` copied `Position` and
  `PrevSize` verbatim into clusters that land at a different Segment position
  - a reader trusting them seeks into the middle of another cluster - and
  `Salvage` kept a cluster's CRC-32 across a clean-cut filter that dropped
  blocks from under it. Hints are now restated in the bytes they already
  occupy (or retired to a Void when the value no longer fits), CRC-32s are
  resealed over what is actually written, and the rollback delta records the
  original bytes as literals wherever the body is no longer verbatim.
- **`ReindexInPlace` accepted two files it cannot repair.** A second, chained
  SeekHead would keep pointing at the Cues the same run had just voided (only
  the head one is rewritten in place), and an unknown-size Segment voids the
  crash-safety window itself - the appended Cues and the journal are supposed
  to live past the Segment's declared end, invisible until the size is
  extended last. Both are now handed to the full reindex, which drops every
  SeekHead and writes one.
- **A half-written rollback entry was reported as no entry at all.** A
  best-effort delta may be dropped silently, but once the first byte has
  landed in the caller's append-only ledger a failure cannot be swallowed:
  the ledger holds the prefix of an entry and has to be truncated. Torn
  writes now reach the caller whatever `RollbackRequired` says.

## [0.21.0] - 2026-07-12

**Highlights**

- **One call, one verdict, one remedy per file - both containers** -
  `diagnose` classifies MKV/WebM (seek-index triage, native per-track
  audio-delay probe, declared-size coherence, tolerant walk only when the
  sizes disagree) AND MP4/MOV (head-only: box-layout truncation, missing
  moov, edit-list audio delays) into the same typed findings, each naming
  its repair (reindex / retime / resync / re-download). One scan loop for a
  mixed library, CLI and WASM routed by the file's first bytes.
- **Stream the broken file anyway (read-only sources)** - PlanHLS gains two
  serving-side repairs that write nothing: `SynthesizeIndex` walks an
  unindexed source once (structure only) and synthesizes the seek index in
  memory instead of refusing, and `AudioPresentationShift` cancels a
  constant A/V desync per track in the init segment's edit list - the
  samples stay verbatim and every media segment is byte-identical with or
  without the shift.
- **"Repair" vs "re-download", said explicitly** - the salvage report now
  carries a first-class `truncated_tail` verdict: damage reaching the end
  of the file is an incomplete source whose tail no tool can restore,
  distinct from repairable mid-file corruption.
- **`retime` now repairs MP4 too** - `mp4.RetimeTracks` (same signature as
  the Matroska op) shifts a track's presentation through its edit list in
  the moov: no sample touched, a few bytes whatever the file size, file-only
  permission, and a crash-ordered moov swap on EVERY layout - the file
  carries exactly one intact moov at every instant. One CLI flag, one map
  shape, both containers, plus a container-agnostic root package
  (`mkvgo.RetimeTracks` / `mkvgo.Diagnose`) that sniffs and routes.
- **The probe JSON is self-sufficient for a library scanner** - every
  derived string is a key (aspect ratios, colour names, resolved language,
  effective sample rate) including `hdr_format`, the one-word
  dolby-vision/hdr10/hlg/sdr classification a tonemap-or-direct-play
  decision keys on. Same shape for MKV/WebM and MP4, CLI and WASM.

### Added

- **`mp4.Diagnose`** (CLI: `diagnose` on an MP4/MOV, routed by content; WASM
  `diagnose` likewise): the head-only MP4 triage returning the same
  `mkv.Diagnosis` shape as the Matroska one. The top-level box layout
  separates `truncated` (a declared box overruns the real end of file -
  present X of Y bytes, re-download) from `trailing-junk`; `no-moov` is the
  nothing-to-rebuild verdict; and each track's edit list yields its
  presentation delay relative to the video anchor - the `audio-delay`
  finding names the exact `retime` invocation, which repairs MP4 too.
  Fragmented sources skip delay derivation. The report types
  (`Diagnosis`/`Finding`/`CueHealthReport`/`SalvageReport`) moved to the
  `mkv` package so both engines share them; `mkv/ops` re-exports type
  aliases, existing consumers unchanged.
- **Root package routers**: `mkvgo.RetimeTracks` and `mkvgo.Diagnose`
  (import `github.com/gravity-zero/mkvgo`) sniff the file's first bytes -
  never the name, so a mislabeled file routes correctly - and dispatch to
  the Matroska or MP4 engine; Matroska-engine-specific options refuse
  loudly on an MP4 source. The `retime` and `diagnose` CLI commands route
  by content the same way.
- **`diagnose` / `ops.Diagnose`**: one-call triage. Classifies a file -
  `no-index`, `index-misskeyed`, `index-stale-tracks`, `audio-delay` (per
  track, with the exact retime invocation), `truncated` (recovered X of Y
  declared bytes), `damaged` (repairable ranges), `trailing-junk`,
  `streamed-size` - and names the remedy on every finding. Composes
  `CueHealth` + `AudioStartDelays` + a head-only declared-size check; the
  full tolerant walk (`MapDamage`) runs ONLY when the sizes disagree. JSON
  carries the full cue-health report, every audio track's delay and the
  damage map when the walk ran. Facade `matroska.Diagnose`; CLI exit 1 on
  findings (scriptable, like `validate`); WASM `diagnose` (Blob-ranged,
  head-mostly).
- **`ops.AudioStartDelays`**: native per-track A/V delay probe - each audio
  track's first block timecode against the first video keyframe (head +
  first cluster(s), bounded, payloads never read; anchor falls back to the
  earliest block on video-less files). Track numbers and delays come from
  the same parse, so the values feed `RetimeTracks` or
  `AudioPresentationShift` directly - no order-based correlation with an
  external prober. Tolerant of truncated/damaged tails: it reports what it
  saw. Facade `matroska.AudioStartDelays`.
- **`mp4.Options.SynthesizeIndex`** (CLI `--synthesize-index` on
  `hls-segment`/`serve`; WASM `synthesizeIndex`): PlanHLS serves a source
  whose Cues are missing or reference no video keyframes by walking the
  clusters once - block headers only via the track-filtered structure-only
  reader - and synthesizing the video cue points in memory. Byte-identical
  to the full pass of the same source; nothing written (the road to
  seekable playback on `:ro` mounts); corrupt streams still refuse.
- **`mp4.Options.AudioPresentationShift`** (CLI `--audio-shift track=ms` on
  `to-hls`/`hls-segment`/`serve`; WASM `audioShiftMs`): re-base audio
  tracks in presentation at packaging time (ns per track, positive =
  content starts late, presented earlier - `AudioStartDelays`' values plug
  in directly). Applied after the fragment timing derivation, so only the
  init segment's edit list moves: media segments are byte-identical with or
  without the shift, full pass and plan agree, over-shifts clamp to the
  presentation start. The ephemeral counterpart of `retime`.
- **`SalvageReport.TruncatedTail`**: first-class truncation verdict on
  `salvage`/`MapDamage`/`Reindex --resync` - damage reaching the end of the
  file marks the source incomplete ("re-download") as opposed to repairable
  mid-file corruption ("repair"). Exposed in JSON (`truncated_tail`), the
  TS types and the diagnose findings.
- **`reader.BlockReader.ClusterOffset`**: absolute file offset of the
  cluster holding the most recently returned block - the base a synthesized
  cue point needs.
- **`mp4.RetimeTracks`** (CLI: `retime` on an `.mp4`/`.mov`, same
  `--shift track=ms` flag): cancel a constant A/V desync in an MP4 by
  editing the track's edit list (`edts`/`elst`) - the empty edit at the
  head of the list IS the track's presentation delay, so the repair is a
  moov-only rewrite: grown, shrunk or created empty edit, track and movie
  durations following, samples and decode times untouched. File-only write
  permission, one crash-ordered landing path for every layout: the new moov
  is appended and synced to disk first, then the old one is retired to a
  `free` box with a single 4-byte type flip - the file carries exactly one
  intact, authoritative moov at every instant (no in-place overwrite of the
  only moov, which a torn write could destroy). A faststart source is no
  longer faststart afterwards; re-run `to-mp4 --faststart` if needed. Explicit refusals: presenting a track
  before the presentation start (MP4 cannot without trimming media),
  unknown tracks, zero shifts, fragmented MP4. The Matroska mode flags do
  not apply and are refused on an MP4 path.
- **Root package `mkvgo`**: container-agnostic entry points that sniff the
  file's first bytes (EBML vs ISO-BMFF - never the name, so a mislabeled
  file routes correctly) and dispatch to the right engine, exactly like the
  CLI. First resident: `mkvgo.RetimeTracks(ctx, path, shift)` routing to
  `matroska.RetimeTracks` or `mp4.RetimeTracks`; Matroska-engine-specific
  options refuse loudly on an MP4 source instead of dropping silently.
- **`mkv.Track.HDRFormat()`**: one-word dynamic-range classification
  (`dolby-vision` | `hdr10` | `hlg` | `sdr`, `""` unknown) from the
  container/bitstream colour signalling already parsed head-only. Dolby
  Vision profile 8 (cross-compatible) classifies by its BASE layer
  (`bl_signal_compatibility_id` 1/2/4 → hdr10/sdr/hlg - it plays without a
  DoVi decoder); only streams that need the DoVi rendering path report
  `dolby-vision`, with the raw DolbyVision fields alongside.
- **Probe JSON derived fields** (CLI `-json` and WASM `probe`, both
  containers): `sample_aspect_ratio`, `display_aspect_ratio`,
  `color_space_name`/`color_transfer_name`/`color_primaries_name`/
  `color_range_name` (conventional prober strings), `hdr_format`,
  `stereo_mode_name`, `resolved_language` (BCP-47 preferred),
  `effective_sample_rate` (decoder rate, SBR applied) - the whole scan
  shape in one call, no post-processing on numeric code points.

## [0.20.0] - 2026-07-12

**Highlights**

- **Fix a constant A/V desync without re-encoding** - `retime` shifts a
  track's timecodes (the repack defect where audio starts late), per track,
  choosing automatically between a 2-bytes-per-block in-place patch under a
  crash-safe journal and a sequential verified rewrite that also rebuilds a
  healthy seek index. Every refusal is explicit; `--rollback-delta` makes it
  undoable for a few hundred KB.
- **Forensic watermarking from ONE source** - `forensic-segment` /
  `openForensic` derive the B variant by dropping a disposable H.264 frame
  per segment, timing-compensated (shared manifest, same durations): A/B
  session watermarking without a second encode.
- **Triage a library's seek health in milliseconds** - `cue-health` reads
  the index head-only and spots files that look indexed but seek wrong
  (cues keyed on audio), with the remedy. Also in WASM, like every
  read-only insight (`mapDamage` joined too).
- **Repairs judge themselves fairly** - the deep verification now compares
  the result against the source and refuses only defects the operation
  ADDED; heritage defects are reported with their remedy instead of
  blocking a correct repair (`--strict` restores the absolute behavior).

### Added

- **`retime` / `ops.RetimeTracks`**: cancel a constant A/V desync (the repack
  defect where audio content starts N ms after the video) by shifting the
  block timecodes of the given tracks in place. Block timecodes are relative
  to their cluster - signed int16, +-32.7 s at the standard 1 ms scale - so
  cancelling a delay of hundreds of ms is a 2-byte patch per block: no
  payload byte moves, no rewrite, no temp file, no disk duplication. The
  patches run under the same crash-safe journal as `reindex-inplace`
  (rollback on any failed check, auto-recovery after a crash) and
  `--rollback-delta` captures them as an undo delta of a few hundred bytes.
  Cluster CRC-32 elements covering patched blocks are recomputed; cues keyed
  on shifted tracks move along. Per-track shifts fix several tracks with
  different delays in one pass. Explicit refusals: sub-resolution shifts,
  int16 overflow, negative absolute timestamps, unknown or block-less
  tracks, cues mixing shifted and unshifted tracks, streamed Segments.
  `Options.DeepVerify` re-walks the result and checks every shifted track's
  first block moved by exactly the requested shift.
- **Single-source forensic watermarking** (`mp4.PlanForensic`,
  `mp4.DropNonRefSample`, CLI `forensic-segment`, WASM `openForensic`).
  A/B session watermarking used to need two pre-encoded variants
  (`PlanWatermark`); a ForensicPlan derives variant B from ONE source with no
  encoder: each variant-B segment has a single disposable H.264 frame
  (`nal_ref_idc == 0` - referenced by nothing, so decoding stays clean)
  removed at the sample level, timing-compensated so the manifest, `#EXTINF`
  durations and the decode timeline of the following segments are
  byte-identical to variant A's. The difference lives in the coded samples
  and survives a remux. `Distinct(n)` reports whether a segment carries a
  bit at all (no disposable frame means identical variants); the serve
  surface mirrors `WatermarkPlan`, so switching between the one-source and
  two-encode flavors is a construction-site change. H.264 only in this
  version.
- **WASM `mapDamage(input, opts?)`**: the read-only damage map (the browser
  twin of `salvage --dry-run`) - diagnose a local, possibly corrupted file
  before uploading it: repaired and lost ranges with byte offsets and
  approximate presentation times, clean-cut cost with `opts.cleanCut`.
  Accepts `Uint8Array` or `Blob`/`File` (ranged reads). TypeScript types
  (`SalvageReport`, `DamagedRange`, `RepairedRange`) included. The repair
  operations themselves stay out of wasm: browser inputs are read-only.

- **`cue-health` / `ops.CueHealth`**: head-only triage of the seek index -
  which tracks the CuePoints reference - from the SeekHead, Tracks and Cues
  alone, in milliseconds. It spots the dormant defect where a video file's
  index exists and is non-empty (every "indexed?" check passes) yet keys on
  audio, so every seek lands mid-GOP. The scan-time complement of
  `validate`, which proves cue times against real keyframes at the cost of
  a full read. Exit 1 when unhealthy; the reason names the remedy.

- **`retime` gained a second engine and picks automatically.**
  `RetimeTracksReplace` reuses the reindex rewrite: timecodes patched on the
  fly during a sequential verified copy, the seek index rebuilt from the
  shifted blocks (healthy video-keyed cues even when the source's were
  not), atomic swap, `--keep-backup`, unknown-size Segments accepted. The
  default `RetimeTracks` measures the file during its scan and uses the
  in-place patch only while it beats a rewrite (each 2-byte patch dirties a
  whole page: on a multi-track movie the dispersed writes cost more than
  copying the file once - a 10-real-files benchmark showed in-place losing
  2-2.5x there while winning 3.5x on short single-track files) or while the
  crash journal fits its cap; beyond either bound it switches engines
  mid-scan. Force with `--in-place` / `--replace`.

### Changed

- **Rollback deltas spool to disk past 8 MiB.** A multi-hour multi-track
  movie's delta (hundreds of MB of 2-byte patches, each paying its op
  framing) used to hit the builder's 256 MiB RAM cap and abandon the entry;
  the ops region now overflows to a temporary spool file next to the output
  and streams to the sink at finalize - same format, one sequential pass,
  no cap on the copy-based operations. The in-place operations keep the RAM
  path (their deltas are bounded by the crash journal's cap, and a spool
  file would break their file-only-permission contract).
- **DeepVerify now diffs instead of gatekeeping.** The result's
  error-severity validation issues are compared against the source's by
  stable identity (`mkv.Issue` gained `Code` and `Track` fields), and only
  an issue the operation ADDED refuses it: a correct repair is no longer
  refused because the file already carried a heritage defect (a muxer's
  mis-keyed cues, subtitles without durations). Preexisting errors are
  reported through `Options.OnPreexisting` / printed by the CLI with their
  remedy; `Options.StrictVerify` / `--strict` restores the absolute
  behavior. Applies to `reindex`, `reindex --resync`, `reindex-inplace` and
  `retime`.
- **In-place operations now require a sync-capable file handle.** The
  crash-safety journal's guarantee is its write barrier (journal durable
  before any patch lands), but the barrier used to degrade to a silent no-op
  on an FS-port handle without `Sync() error` - while the less
  safety-critical `Truncate` was a hard requirement. The criticality is now
  the right way up: `ReindexInPlace`, `RetimeTracks` and the journal
  recovery refuse a handle that cannot sync, before touching anything.
  `mkv.MemFS` declares its semantics with an explicit no-op `Sync` (writes
  hit the backing slice immediately; there is no more durable medium).
- **Finished files now declare their Segment size.** `MKVWriter.Finalize`
  seals the Segment's size (previously left as the unknown-size marker), so
  every output - mux, merge, reindex, salvage, WebM - states its extent the
  way mainstream muxers do. This is what lets the in-place operations hide
  their transient journal past the declared end; files written by earlier
  versions are still read fine, but `retime` refuses them (unknown size)
  until they are reindexed once.
- **`SalvageReport` JSON keys are now snake_case** (`clusters_copied`,
  `damaged_ranges`, ...), aligned with `analyze`/`ingest`/`fingerprint` for
  one consistent JSON surface across CLI `--json` and wasm.

## [0.19.0] - 2026-07-11

**Highlights**

- **Repair, not refuse** - `reindex --resync` (opt-in) repairs a file whose
  cluster stream is corrupted instead of refusing it: lying size fields over
  intact payloads are corrected with zero loss, and damage inside a cluster
  is cut around block by block (chain-validated against the file's real
  tracks and timecodes), so a repair typically loses a few KB where a plain
  skip-to-next-cluster would lose seconds of media. The strict default is
  unchanged.
- **Decide before you touch anything** - `salvage --dry-run` maps the damage
  without writing: repaired ranges, lost ranges with byte offsets and
  approximate presentation times, clean-cut cost - exactly what the real
  repair would report.
- **Undo without a backup copy** - every repair can emit its inverse delta
  (`--rollback-delta`, `Options.RollbackSink`): the recipe to reconstruct
  the pre-repair original from the repaired file, typically well under 0.1%
  of its size. `mkvgo rollback` applies it, hash-gated twice.
- **Clean cuts** - `--clean-cut` resumes video at the next keyframe after a
  damage gap (audio immediately), trading a short freeze for
  reference-broken artifacts.

### Added

- **Resync option for `reindex`** (`Options.Resync` / `--resync`, opt-in). Some
  files play everywhere yet were refused by `reindex`: a corrupted region in
  the cluster stream (a declared size that does not match the element's real
  extent, damaged bytes inside a cluster body, raw junk between clusters)
  makes the strict top-level walk land mid-payload and read garbage as an
  element ID, where players simply resynchronize and carry on. With `Resync`
  set, `Reindex` and `ReindexReplace` tolerate the damage, rebuilding SeekHead
  and Cues from what survives. Every dropped source range is reported (byte
  offsets plus approximate presentation time) through `Options.OnSkip` and
  the CLI summary; every reconstructed region through `Options.OnRepair`.
  The repair is refused when no anchor is found within the bounded scan
  window (64 MiB), when no cluster survives, or when more than half of the
  walked payload would be dropped - a mostly-damaged file must not silently
  "repair" into a stub (`salvage` remains the uncapped best-effort path). A
  clean source produces output byte-identical to a strict reindex; the strict
  default is unchanged and its refusal message now points at the option.
  `ReindexInPlace` refuses the option (skipped bytes cannot be dropped from
  the file itself). `DamagedRange` now lives in the `mkv` package
  (`ops.DamagedRange` is a compatible alias).
- **Surgical recovery inside damaged clusters** (shared by `salvage` and
  `reindex --resync`). A damaged cluster used to be dropped whole - its
  declared extent, up to hundreds of KB or more, for what is typically a few
  KB of real damage. The tolerant walk now re-derives the truth from the
  bytes: it walks cluster children from the body start ignoring the declared
  size, and on a break scans forward for the next chain-validated block
  (known child IDs, in-bounds sizes, track numbers from the file's real
  track set, timecode continuity across the gap), splitting the cluster
  around the damage instead of discarding it. A lying size field over an
  intact payload is repaired with zero loss; damage inside a body loses only
  the unrecoverable bytes. Continuation runs are emitted as clusters
  carrying the original cluster's Timestamp, so recovery never guesses
  timing - a region whose Timestamp cannot be read is not block-recovered at
  all. Reconstructed regions are reported as `RepairedRanges` (with the
  bytes a plain skip would have lost). An unknown top-level element is now
  copied only when a validated known element (or EOF) starts exactly where
  it claims to end - garbage can no longer masquerade as a chain of
  "unknown elements" and ride verbatim into a repaired output. Fuzz target
  on the surgical scanner.
- **`Options.CleanCut` / `--clean-cut`** (opt-in, `salvage` and
  `reindex --resync`): after a damage gap, the first recovered video frames
  are often P/B frames referencing lost pictures - they decode with
  artifacts until the next keyframe. Clean cut drops video until that
  keyframe (audio resumes immediately; its frames are independent), turning
  "it glitches for a while" into a clean jump. The dropped bytes are
  reported (`CleanCutBytes`) and the damaged range's end time extends to the
  resume keyframe.
- **`salvage --dry-run` / `ops.MapDamage`**: the exact salvage walk with
  nothing written - a damage map an operator can read before deciding to
  repair ("this file would lose 3.9 KB at 00:00-00:02"), with repaired
  ranges, damaged ranges and clean-cut cost, identical to what the real run
  would report.
- **Rollback delta** (`Options.RollbackSink` / `--rollback-delta <file>`,
  `mkvgo rollback`, `ops.ApplyRollback`): a repair can now emit the recipe to
  reconstruct the pre-repair original from the repaired output - typically
  well under 0.1% of the source where a backup copy would be the whole file.
  The writer already knows the src->dst mapping of every verbatim run it
  copies, so the delta (framed entry: COPY ranges of the repaired file,
  literals for what the repair dropped or re-encoded, crc32c per entry) costs
  no diff pass; only the repaired file's sha256 adds one sequential read.
  Supported by `reindex` (strict and `--resync`), `reindex-inplace` (the
  crash journal persisted as a delta, emitted while it can still be rolled
  back) and `salvage`. Application is hash-gated twice: `mkvgo rollback`
  refuses when the repaired file changed since the repair, and never
  delivers a reconstruction that does not hash back to the original.
  `Options.RollbackRequired` decides whether a sink failure fails the repair
  (the CLI flag implies it); the default is best-effort. The summary comes
  back through `Options.OnRollback` (`mkv.RollbackInfo`). Fuzz target on the
  entry parser.

## [0.18.0] - 2026-07-10

**Highlights**

- **AES-128 key rotation** - `Options.Encrypt` can rotate the key every N
  segments (forward secrecy: a captured key decrypts only its period, not the
  whole video), emitting a fresh `EXT-X-KEY` at each boundary. The schedule is a
  pure function of the segment index, so `RemuxToHLS` and `PlanHLS` stay
  byte-identical. CLI `--aes-rotate-segments`, WASM `encrypt.keys`.
- **Common Encryption for AV1 and VP9** - CENC subsample encryption now covers
  AV1 and VP9 alongside H.264/HEVC, opening the DRM/EME (DASH) path to all-AV1
  and VP9 audiences. AV1 parses `frame_header_obu()` against the segment
  sequence header (combined `OBU_FRAME`); VP9 keeps each frame's uncompressed
  header clear, resolving inter frames' reference dimensions from the segment
  keyframe. Both were validated decrypting and decoding in a Clear Key player,
  and refuse (rather than mis-protect) frame constructs their parsers do not yet
  cover.
- **Forensic A/B session watermarking** - `PlanWatermark` serves one HLS
  presentation whose per-segment bytes are drawn from one of two GOP-aligned
  encodes by a per-viewer bit pattern, so a leaked copy carries a signature
  identifying the session. No re-encode; the manifest is shared. CLI
  `watermark-segment`, WASM `openWatermark`. The code assignment stays the
  caller's policy.
- **AES-128 whole-segment encryption in WebAssembly** - `openHLS`/`openABR`
  accept an `encrypt` option (the counterpart of the CLI `--aes-key` flags), so
  browser packaging can produce encrypted HLS.
- **Cross-container content fingerprint** - `fingerprint` now accepts MP4/MOV
  and yields per-track digests identical to the same encode as MKV/WebM, for
  upload deduplication across formats.

### Fixed

- **`EXT-X-MAP` now precedes `EXT-X-KEY` in the media playlist.** Per RFC 8216 a
  key applies to every `EXT-X-MAP` that follows it, but the init segment is
  always clear; emitting the key first made players retry-loop trying to decrypt
  the clear init (an `hls.js` `fragDecryptError`). A regression test and a
  structural no-dangling-reference guard were added.
- **VP9 RFC 6381 codec string.** A VP9 stream with no CodecPrivate (the common
  WebM case) emitted an empty or level-0 `CODECS`, which players reject; the
  record is now derived from the first-frame sample entry and the level computed
  from the picture size.

### Internal

- Serving load/capacity anti-regression suite: a machine-independent proof that
  serving a segment is O(segment) not O(source) (allocations do not grow with
  source length), the retained memory per idle stream, and byte-identical output
  under concurrent serving. `make bench` runs the throughput benchmarks.
- Raised `mp4` mutation-test efficacy to 100% on covered mutants, added real
  (synthetic-pattern) AV1/VP9 bitstream fixtures that exercise the codec parsers
  end to end, and a cross-platform preflight gate (`make preflight`).

## [0.17.1] - 2026-07-09

### Changed

- **Box and index payloads read incrementally.** When a source declares an
  element larger than it actually delivers - notably a hostile remote
  Content-Length, where the "declared size fits the file" guard is only as
  trustworthy as the server's reported length - the MP4 moov/box and Matroska
  Cues reads now grow the buffer as bytes arrive rather than allocating the
  declared size up front, so the read fails at I/O time without a large
  single-shot allocation.

### Documentation

- Corrected the binary and WebAssembly size figures (the static binary is about
  8 MB; the WebAssembly module gzips to about 1.6 MB) and removed unverified
  comparative claims.
- Documented that Common Encryption key/IV uniqueness across separate encodings
  is the caller's responsibility: mkvgo validates the key/IV lengths, not their
  global uniqueness, and reusing a key with the same base IV across encodings
  reuses the keystream.

### Internal

- Raised `matroska` facade test coverage to 100% and factored two long reindex
  functions into smaller behavior-preserving helpers.

## [0.17.0] - 2026-07-09

**Highlights**

- **One-call onboarding** - `ingest` composes analysis, the playability verdict
  and an in-place reindex into a single serving plan (direct-play / remux-HLS /
  transcode), so a media server decides how to serve a file in one call instead
  of stitching the steps together.
- **Direct-play serving** - `mkvhttp.FileHandler` and `serve --direct/--auto`
  stream the raw file over HTTP byte-range for clients that play it as-is, so the
  same server does direct-play and on-demand HLS, chosen by playability.
- **Chapter markers** - opt-in `--chapter-markers` exposes source chapters as HLS
  `EXT-X-DATERANGE` and DASH `EventStream` for chapter navigation and ad-insertion
  points, with the media segments byte-identical whether it is on or off.
- **Stream analysis without decoding** - `analyze` reports exact per-track frame,
  packet and keyframe counts, GOP structure, windowed bitrate and true duration
  from a header-only walk (payloads are seek-skipped), so the cost scales with
  the block count, not the media size.
- **Playability verdicts** - `playability` decides, from head-only metadata,
  whether a file direct-plays, needs only a container remux, or needs a
  transcode on a given target (Safari, the Chromium family incl. Chrome / Edge /
  Brave / Opera / Vivaldi, Firefox, Chromecast, generic MSE), and `ladder`
  recommends an ABR rung set - all without decoding.
- **Play while downloading** - `serve-growing` streams a still-downloading file
  as HLS: an EVENT playlist that grows with the file and finalizes to VOD, with
  every already-covered segment byte-identical to the finished file.
- **Gapless multi-file sessions** - `concat-hls` packages several sources (a
  season of episodes) as ONE continuous HLS session: the player never reloads
  across boundaries.
- **Common Encryption (CENC)** - `cenc` (AES-CTR) and `cbcs` (AES-CBC pattern)
  sample-level packaging with `SAMPLE-AES`/`SAMPLE-AES-CTR` signaling and DASH
  `ContentProtection`, ready for EME/DRM playback (keys stay the caller's job).
- **Virtual subtitle resync** - `--sub-offset` shifts every cue by a chosen
  delay, served on the fly, no file rewritten.
- **Damaged-file recovery** - `salvage` copies what is readable out of a
  corrupt MKV, skipping dead regions and reporting the damaged time ranges.
- **Serve over HTTP** - the `mkvhttp` handler and `mkvgo serve` expose an
  on-demand HLS/DASH plan (ETag, Range, caching) in a few lines; the `s3fs`
  port streams a source straight from an S3 bucket, no download.
- **In-place reindex** - `reindex-inplace` patches the seek index directly into
  the file: no copy, no temp file, write permission on the file is enough, and
  a crash-safety journal inside the file makes any failure roll back to the
  original bytes.
- **Verified reindex** - every reindex now re-opens and checks its own result;
  `--deep-verify` adds a full-read validation plus a byte-level payload proof,
  and `reindex --replace` swaps the verified copy over the original atomically.

### Added

- **One-call serving plan (`Ingest` / `ingest`).** Composes the building blocks
  into a per-file decision for a media server: it runs the playability verdict
  against a target and returns a `ServingPlan` - `direct-play` (serve the source
  as-is), `remux-hls` (package on-demand HLS without transcoding; it also reports
  whether the source already has a usable seek index and, with `-reindex`,
  repairs one in place, falling back to a copy reindex via
  `ErrIndexNotHeadDiscoverable` when the layout does not allow it), or
  `transcode` (with a recommended `ladder`). Optionally attaches the full
  `analyze` report. Head-only unless a reindex or analysis is requested. CLI
  `mkvgo ingest [-target <name>] [-reindex] [-analyze] [-json]`.
- **Direct-play HTTP serving (`mkvhttp.FileHandler`, `serve --direct` /
  `--auto`).** A handler that streams a single local file over HTTP with full
  byte-range support (seeking/scrubbing), a strong O(1) ETag (from size and
  mtime, never the file content), the right Content-Type, and an immutable cache
  header - for clients that direct-play the source. `serve --direct` serves the
  raw file; `serve --auto` runs the playability verdict and picks direct-play or
  the on-demand HLS plan automatically, printing which it chose.
- **Chapter markers in HLS/DASH (`Options.ChapterMarkers` / `--chapter-markers`).**
  Opt-in: exposes the source's chapters as one HLS `EXT-X-DATERANGE` per chapter
  (title, start, duration) and a DASH `EventStream`, for chapter navigation and
  ad-insertion points. Off by default and byte-identical to before when off; when
  on, only the manifest/playlist text gains marker lines - the media segments are
  byte-identical either way, and the markers are emitted identically by the full
  pass and the on-demand plan.
- **Frame-rate mode in `analyze` (CFR/VFR).** Each video track now reports
  `FrameRateMode` (`cfr`/`vfr`) and a frame-duration variance, detected from the
  timecodes the header-only walk already reads (no decode, memory-bounded); a
  variable-frame-rate track also raises a warning.
- **Content fingerprint (`Fingerprint` / `fingerprint`).** A container-independent
  content identity: a stable hash over every track's payload digest (in decode
  order, tracks sorted by a content key so track reordering does not change it),
  so a library can dedup re-muxes of identical content across MKV/WebM containers.
  Reuses the per-track digest the block comparison relies on; this reads payloads
  (unlike the head-only `analyze`). CLI `mkvgo fingerprint [-json]`.
- **Stream analysis (`Analyze` / `analyze`).** A single header-only walk that
  reports, per track, exact frame and packet counts (lacing expanded), keyframe
  count and GOP structure (min/max/avg frames between video keyframes, average
  keyframe interval), average and peak bitrate (over a one-second sliding
  window), total bytes, and the true duration from the last frame; plus
  container totals (cluster and block counts, overall bitrate) and timing
  sanity warnings (declared vs true duration mismatch, non-monotonic timecodes,
  zero-frame or duration-less tracks). Payload bytes are seek-skipped, so it
  never decodes and its cost is proportional to the block-header count rather
  than the media size. Memory stays bounded (counters plus a one-second window).
  Matroska/WebM sources in this version. CLI `mkvgo analyze [-json]`.
- **Playability verdicts (`Playability` / `playability`).** From head-only
  metadata alone, a per-track and overall verdict - `direct-play`, `remux`
  (with the cheapest container that would play), or `transcode` (with the
  reason) - for a target device or browser. Built-in targets: `safari`,
  `chrome` / `chromium-generic` / `brave` / `opera` / `vivaldi` /
  `samsung-internet` (shared Chromium baseline), `edge` (Chromium plus HEVC via
  the OS extension), `firefox`, `chromecast-gen3`, `mse-generic` (the default,
  the universal H.264 High@4.1 + AAC bar). The capability table is plain data a
  caller can override with a custom `Target`. Codec, profile, level, bit depth,
  resolution, HDR and Dolby Vision are all read head-only; a missing field with
  a matching constraint yields a conservative `transcode`, never a guessed
  direct-play. CLI `mkvgo playability [-target <name>] [-json]`.
- **ABR ladder recommendation (`RecommendLadder` / `ladder`).** Suggests a
  bitrate-ladder rung set from a source's resolution, bitrate and codec, capped
  at the source (never upscales, never exceeds the source bitrate) and scaled by
  a per-codec efficiency factor. Guidance for configuring an encode, computed
  from metadata only. CLI `mkvgo ladder [-json]`.
- **Play while downloading (`PlanGrowingHLS` / `serve-growing`).** Serve a
  still-growing (downloading) MKV as an EVENT-type HLS playlist that lengthens
  as data lands: a cursor scans only whole clusters (a partial trailing cluster
  is held back until complete, so a truncated segment is never served), each
  published segment is byte-identical to the finished-file `PlanHLS` build and
  keeps a stable number, and the playlist switches to VOD + `EXT-X-ENDLIST` on
  completion (explicit `Complete()` or an auto-detected finalized file).
  `Refresh` is safe to call concurrently with `Resource`. Encrypted growing
  plans are refused in this version.
- **Gapless concatenation (`PlanConcat` / `RemuxConcatToHLS` / `concat-hls` /
  `concat-segment`).** Package several MKV/MP4 sources as one continuous HLS
  session: each part keeps its own byte-identical segments, joined by
  `EXT-X-DISCONTINUITY` and a per-part `EXT-X-MAP` (playlist version 6), so a
  binge session plays across episodes with no player reload. Subtitles are
  concatenated with each part's cues shifted by the cumulative prior duration
  when every part shares the same subtitle layout, otherwise dropped with a
  reason. Sources must share a compatible video codec and audio layout (an
  explicit error lists any mismatch before anything is written); combined DASH
  and per-concat I-frame playlists are out of scope in this version.
- **Common Encryption (`Options.CENC` / `CENCOptions` / `--cenc-*`).**
  Sample-level `cenc` (AES-CTR) and `cbcs` (AES-CBC 1:9 pattern) encryption of
  the fMP4/HLS/DASH output with caller-supplied keys: `encv`/`enca` sample
  entries, `tenc`/`senc`/`saiz`/`saio` boxes, subsample encryption for H.264 /
  HEVC (NAL length + header left clear), whole-sample for audio, `EXT-X-KEY`
  (`SAMPLE-AES` / `SAMPLE-AES-CTR`) and DASH `ContentProtection` signaling, and
  optional `pssh` passthrough. The per-sample IV derivation is byte-identical
  between the full pass and the on-demand plan. Packaging only - no license
  server; key delivery is the caller's. AV1/VP9 and `SingleFile` are refused in
  this version.
- **Virtual subtitle resync (`Options.SubtitleOffsetMs` / `--sub-offset`).**
  Shift every WebVTT cue by a positive or negative millisecond offset in the
  served renditions (full pass and on-demand plan alike), with cues clamped or
  dropped at zero. Nothing is written to the source: a server re-plans with a
  new offset instantly. Zero offset is byte-identical to before.
- **I-frame trick-play for Matroska sources.** On-demand `PlanHLS` now serves
  `iframe.m3u8` for MKV/WebM sources too (previously MP4 only), built lazily on
  first request from a bounded header-only pass over the video track so plan
  construction stays cheap; byte-identical to the full pass.
- **Damaged-file recovery (`Salvage` / `salvage`).** A separate, explicitly
  lossy-tolerant copy: it walks clusters like `reindex` but on structural
  corruption (garbage, zeroed regions, a truncated tail) it resyncs forward to
  the next valid cluster (bounded scan), skips the dead range and resumes,
  rebuilding the index from what survives. Returns a report of the damaged byte
  and time ranges; the strict `reindex`/`validate`/`BlockReader` contracts are
  untouched. A clean file yields zero damaged ranges.
- **HTTP serving (`mkvhttp` package / `mkvgo serve`).** A drop-in
  `http.Handler` around any plan's `Resource` method: GET/HEAD, strong SHA-256
  ETag with conditional `304`, `Range` via `http.ServeContent`, per-class
  `Cache-Control` (playlists `no-cache`, immutable segments cached), optional
  CORS. `mkvgo serve <file>` plans a file and serves it with graceful shutdown.
- **S3 source port (`s3fs` package).** A read-only, range-backed FS port that
  signs each request with AWS Signature V4 (standard library crypto only) over
  the existing `httpfs` windowing, so any command reading `http(s)://` now also
  reads `s3://bucket/key` (virtual-host or path style, credentials/region from
  the environment) without downloading the whole object.
- **WASM bindings for the new streaming features.** `MkvGo.openConcat(inputs,
  opts)` packages several in-browser sources into one continuous HLS session;
  `subOffsetMs` (virtual subtitle resync) is honoured by `openHLS`/`openABR`/
  `openConcat`; and a `cenc` option ({scheme, key, keyId, iv, keyURI}) drives
  Common Encryption from `openHLS`/`openABR`. TypeScript types updated.
- **`ReindexInPlace` / `reindex-inplace`.** Surgical index repair: the new Cues
  element is appended inside the Segment, the Segment size extended, the head
  SeekHead repointed and any stale Cues voided - cluster bytes never move and
  no second copy of the file is created. Crash-safe by design: the bytes about
  to be overwritten are journaled inside the file (fsynced) before any patch,
  the result is verified while a rollback is still possible, and the journal is
  truncated away only after the checks pass. An interrupted run is repaired
  automatically by the next one, or explicitly via `RecoverInPlace` /
  `--rollback`. Streamed (unknown-size-cluster), truncated and
  no-SeekHead-slot files are refused with a pointer at `reindex` (copy).
- **`ReindexReplace` / `reindex --replace [--keep-backup]`.** Rebuild through a
  temporary copy in the same directory, verify, then atomically rename it over
  the original - the source is never touched until every check has passed.
  `--keep-backup` keeps the pre-op file as `.bak`.
- **Reindex verification built in.** Every `Reindex` re-opens its output and
  checks the written Cues against the index built during the copy (head-only,
  milliseconds). `Options.DeepVerify` / `--deep-verify` adds a full-read
  `Validate` (every cue checked against real video keyframes) and a byte-level
  payload comparison against the source (`CompareBlocks`).
- **FS port: `Rename` hook and MemFS `Truncate`** so the new operations run on
  any filesystem port, in-memory included.

## [0.16.0] - 2026-07-05

**Highlights**

- **Virtual Edit Layer** - serve "VF only" / "VO + English subs" / "clean"
  versions from one file, no copy (`--keep-tracks` / `--keep-lang`).
- A `reindex` output now passes its own `validate` on time-based repacks (the
  Avatar class of files).

### Added

- **Virtual Edit Layer (`Options.KeepTracks`).** Serve many virtual versions of
  one source - "VF only", "VO + English subs", "clean" (drop a logo/forced
  track), a chosen camera angle - from a single file with no copy and no re-mux:
  `KeepTracks` restricts the presentation to a subset of Matroska track IDs, so
  `PlanHLS`/`RemuxToHLS`/`RemuxToABR` package only those renditions (the on-demand
  plan applies the subset byte-identically to the full pass). Wired through the
  CLI - by ID (`--keep-tracks 1,2`) or by language (`--keep-lang fre`, CLI sugar
  that keeps the video plus every track matching the codes) - and WASM
  (`{ keepTracks: [...] }`).

### Fixed

- **`ReindexInPlace` no longer reports success with a seek index that is not
  usable for seeking.** On a file whose only in-place patch slot forced the
  rebuilt SeekHead after the Info/Tracks elements (a source with no head
  SeekHead), the appended Cues were recoverable by a full read (which scans
  back from the file end) but not by a head-only seeker following the SeekHead -
  yet the operation reported success. The built-in verification now also checks
  that the index is discoverable head-only; when it is not, the in-place patch
  is rolled back (the file is left byte-identical) and the caller is pointed at
  a full reindex (copy), which always writes a head-discoverable index.
- **A reindexed file no longer fails its own `validate`.** `reindex` emitted a
  fallback cue on the first (often audio) block of any cluster that held no video
  keyframe - common in time-based cluster splits (~38% of clusters on some
  repacks) - while `validate` flags a cue on a non-video track as a blocking
  error ("seeking lands on audio"). The two contradicted, so such a rebuild could
  never pass validation. reindex now cues only real video keyframes for a file
  with video (a Cues index needs no per-cluster entry); the throttled fallback
  remains for genuinely audio-only files, where there is no keyframe to key on.

## [0.15.0] - 2026-07-04

**Highlights**

- **Multi-quality ABR** - on demand (`PlanABR`), in the browser (WASM `openABR`),
  with per-variant trick-play and a combined DASH manifest for aligned variants.
- **Fragmented-MP4 / CMAF input** - a pre-encoded/streaming fMP4 ladder is read
  like a progressive file.
- **In-browser HLS origin (Service Worker)** - stream a local file far larger
  than memory in a plain `<video>`/hls.js, no server, no upload.
- **Laced-audio timing fixes** - E-AC-3/AC-3 rips no longer decode with broken,
  non-monotonic audio after a remux.

### Fixed

- **Inspection commands read a mislabeled container.** A `.mkv` that is actually
  an MP4/MOV (mislabeled rips happen) failed `info`/`tracks`/`probe`/… with a
  cryptic EBML-header error; they now route to the mp4 reader transparently when
  the bytes are ISO base media.

- **Packaging tolerates a source that runs past the physical end of file.** A
  truncated or unfinalised file whose Segment/final cluster over-declares its
  size made `to-hls`/`to-mp4`/`PlanHLS` abort with a fatal `unexpected EOF`. The
  packaging walks now treat a short read at the true EOF as a clean end (every
  complete block delivered, only the truncated tail dropped); the strict
  `BlockReader` still reports it so `validate`/`compare` can flag it.
- **Laced audio frames are individually timed.** A laced block stores N frames
  under one timecode; the reader gave every frame that timecode, so consumers
  deriving durations from timestamp deltas got collapsed, non-monotonic times
  (broken fMP4/HLS/MP4 audio, players freezing after a seek). Frame i is now
  stamped `blockTS + i×DefaultDuration`, and constant-rate audio rides its exact
  sample grid (fixed stride, no ±1 ms jitter, no drift). When the source
  declares no DefaultDuration (common for E-AC-3/AC-3 rips) the packager
  recovers the stride from the block timecodes themselves - exact for the
  whole-millisecond frames of AC-3/E-AC-3 - across MP4, HLS and the on-demand
  plan. The lace keyframe flag now applies to every frame of the lace.
- **EBML lacing had an off-by-one signed-VINT bias corrupting every EBML-laced
  block.** The inter-frame diff is `value − (2^(7·n−1) − 1)` (RFC 9559); the
  reader subtracted `2^(7·n−1)`, shifting every frame boundary after the first
  (total intact, so size checks passed - but the audio was undecodable after a
  remux). Frame sizes now match the source exactly.

### Added

- **Multi-quality (ABR) on demand and in the browser.** `mp4.PlanABR` serves a
  multi-variant HLS presentation from several pre-encoded quality variants with
  nothing pre-generated: one `Resource()` resolves "master.m3u8" or any
  "v{k}/<name>", byte-identical to `RemuxToABR`. The WASM `openABR(inputs[])`
  exposes the same over Uint8Array or Blob variants (ranged reads, so a
  client-side ladder of huge local files stays memory-bounded). The `abr-segment`
  CLI serves one ABR resource on demand; the ABR master advertises per-variant
  I-frame (trick-play) streams for CMAF/MP4 ladders. When the variants are
  segment-aligned, a combined DASH `manifest.mpd` (one AdaptationSet, a
  Representation per variant) is emitted so a single manifest switches quality;
  misaligned variants keep their own per-variant manifests.
- **In-browser HLS origin (Service Worker).** A small fetch router turns
  `openHLS`/`openABR` into an origin, so a plain `<video>`/hls.js streams a
  local file - even one far larger than memory - with no server and no upload
  (`web/example/hls-sw.html`). Each on-demand resource now carries a
  deterministic content `sha256`, a ready-made `ETag` for HTTP caching / CDN
  dedup.
- **Fragmented-MP4 / CMAF input.** A fragmented source (streaming fMP4, DASH/HLS
  CMAF - the shape most pre-encoded ladders take) is now read by walking its
  moof/traf/trun fragments into a full sample table, so `from-mp4`, `to-hls`,
  `to-abr` and the plans treat it like a progressive file (previously rejected).
- `Track.DefaultDurationNs`: the raw DefaultDuration in nanoseconds, kept for
  every track type (previously only exposed as the video `FrameRate`). The
  writer round-trips it verbatim, so rewrites no longer drop an audio track's
  declared frame duration.
- `Block.BlockTimecode`: the enclosing block's stored timecode (equal to
  `Timecode` for unlaced frames), letting consumers partition laced frames
  without splitting a lace.
- `BlockReader.SetTrackDefaultDurations` + `matroska.TrackDefaultDurations`
  helper (see docs/library.md, Block-level access).

## [0.14.0] - 2026-07-03

### Added

- **Windowed subtitle segments are served from an incremental cue scan - the
  first request is sub-second, like a video segment.** `HLSPlan` used to
  produce a subtitle rendition's cues in one whole-file pass on the first
  request (tens of seconds on a multi-GB source - the delay behind the
  first-hit 404s on slow storage). The pass is now a per-track cursor:
  serving `subN_%05d.vtt` scans the source only up to that segment's end
  (plus the relative-timecode margin blocks can be stamped within, and any
  successor a duration-less cue's end resolves on), commits its progress, and
  the next request resumes where it stopped. Playback order therefore pays
  one bounded prefix read per segment and at most one full pass in total;
  requests in any order, cancellations mid-scan and the whole-track
  `subN.vtt` stay byte-identical to the full pass. Built on two new
  `reader.BlockReader` primitives: `StopBeforeClusterMs` (bound a walk by
  cluster timestamp - it also stops a track-filtered walk from skimming to
  EOF hunting a sparse track's next block) and `ResumeOffset` (resume a
  bounded walk exactly where it stopped).
- **A cold seek into the middle of the presentation costs O(window) too.**
  The incremental cue scan alone still paid O(position) on the first request
  after a far jump (it extended the prefix from the track start). A far
  windowed request now seeks through the segment index to a bounded island:
  scan from the cluster covering the window start minus a backward margin  - 
  the relative-timecode spread plus the longest cue the fast path accounts
  for (60 s) - forward to the window end, and slide the island on subsequent
  requests; a later far jump re-seeks a fresh island instead of dragging the
  old one across the gap. The fast path applies only when the subtitle
  blocks observed (a one-stride track-head probe, plus every scan) carry
  explicit durations within the margin - real-world subtitle muxing; tracks
  with duration-less or over-long cues fall back to the always-exact prefix
  scan. Windowed outputs stay byte-identical to the full pass on both paths.

### Changed

- **The on-demand subtitle-cue pass skips media payloads it walks past, and
  the block reader's I/O adapts to the source's block sizes.** `HLSPlan`'s
  lazy cue collection needed one sequential pass over the whole source (also
  the window in which a client disconnect used to poison the cue cache, see
  below). The block reader gained a track filter
  (`reader.BlockReader.KeepTracks`) built on an adaptive read window: a skip
  past the window is seeked over - never read - when it is large enough to
  beat the fixed per-read round trip remote filesystems charge (~64 KiB);
  smaller skips read forward through a window that doubles on sequential
  fills, so the walk becomes one bulk chunked read, never one small read per
  block. In practice: sources with large frames (remuxes) load cues reading a
  few percent of the file; sources with small interleaved frames (typical
  1080p/4K encodes, ~7-40 KiB per block) are bounded by one sequential pass  - 
  now in large chunks, where a fixed small buffer degraded to per-block round
  trips and ran slower than a plain full read on 9p/SMB mounts. Large skips
  of non-block elements (Cues, Attachments) also seek instead of reading, and
  a skip beyond EOF surfaces as an error instead of a silent clean end.

### Fixed

- **`hls-segment -o -` wrote a file literally named `-`.** The output flag
  now treats `-` as stdout, like the other commands.
- **A cancelled request poisoned an on-demand subtitle track for the plan's
  lifetime.** `HLSPlan`'s lazy subtitle-cue pass ran under the requesting
  client's context and cached its result through a `sync.Once` - if the client
  disconnected mid-scan (the pass reads the whole source, seconds on a large
  file), `context.Canceled` was cached and every later request on that track
  failed instantly, which a server maps to a deterministic 404. Context
  cancellation and deadline errors are now treated as transient: only success
  and permanent errors are cached, and the next request with a live context
  re-runs the scan.

## [0.13.0] - 2026-07-02

### Fixed

- **Rewrites dropped the declared frame rate.** The writer never emitted
  `DefaultDuration`, so every rewrite (mux/merge/edit/split/remux) silently
  lost the source's nominal frame rate; it now round-trips via
  `Track.FrameRate`. Surfaced by the new `validate` streaming-readiness check.
- **The writer cued audio in mixed clusters.** `WriteClusterWithCues` keyed
  the Cues on the cluster's first keyframe-flagged block - and every audio
  block carries that flag, so files written by mux/merge/edit carried cue
  times naming an audio block instead of the video keyframe (the same bug
  class as the 0.12.0 reindex fix, here on the write side). Cues now key on
  video keyframes; a video file's cluster without one is not cued (a mid-GOP
  cue is a false seek target), and audio-only files keep the throttled
  first-block cues.
- **`merge-subtitle` kept no cue durations.** The SRT/ASS end times were not
  written as BlockDurations, so every merged cue's length was lost (readers
  fell back to guessed durations). The end time now rides as the
  BlockDuration, so extraction and HLS WebVTT renditions reproduce the exact
  source timing.

### Added

- **DASH output and demuxed CMAF renditions.** The packaging is CMAF proper:
  one demuxed rendition per track - the video (`playlist.m3u8`, `init.mp4`,
  `seg00001.m4s` …) and each audio track (`audio1.m3u8`, `init_a1.mp4`,
  `seg_a1_00001.m4s` …, an `EXT-X-MEDIA` AUDIO group) - served through two
  manifests over the same segments: HLS (`master.m3u8`) and **DASH**
  (`manifest.mpd`, static VOD, one AdaptationSet per rendition with an exact
  `SegmentTimeline`). Multi-audio sources (VF/VO) get native language
  selection in hls.js/Safari/dash.js, and DASH players - which reject muxed
  representations - are first-class. Movie metadata (title, tags, cover art)
  rides on the video init; a track that ends early keeps its rendition
  aligned with empty fragments; secondary video tracks are dropped with a
  reason. Both serving modes (`to-hls` and `hls-segment`/`PlanHLS`) emit the
  layout byte-identically (regression-tested per rendition, decoder-verified
  for both manifests).
- **Fragmented-MP4 / CMAF HLS output** (`mp4.RemuxToHLS`, CLI `to-hls`). Remuxes
  an MKV/WebM into a complete HLS presentation - `master.m3u8` (multivariant
  playlist with `BANDWIDTH`/`RESOLUTION` and RFC 6381 `CODECS`), the muxed
  audio+video media playlist, `init.mp4` (ftyp + moov with `mvex`/`trex` and
  empty sample tables) and `styp`/`moof`/`mdat` media segments. Text subtitle
  tracks (SRT, WebVTT, ASS/SSA flattened) ride as segmented WebVTT renditions
  declared in the master playlist (language/name/default/forced); bitmap
  subtitles are dropped with a reason. No transcoding: samples are copied
  verbatim into CMAF fragments (H.264/HEVC/AV1/VP9 + AAC/…). Segments are cut
  on video keyframes at roughly `Options.SegmentMs` (default 6 s) and are
  independently decodable (random access); audio gapless priming (CodecDelay)
  is re-signalled as an edit list in the init segment, like the progressive
  remux. Memory is bounded - per-sample metadata in RAM, sample bytes streamed
  through one temp file per track. This is the CMAF "copy rung" of an HLS
  ladder; bitrate variants (real ABR) remain a transcoder's job.
  decoder-verified: exact frame parity with the source and standalone
  mid-stream segment decode.
- **On-demand HLS** (`mp4.PlanHLS`, CLI `hls-segment`). The zero-storage
  counterpart of `to-hls`: `PlanHLS` reads the metadata head (with its Cues),
  the first and the last cluster - a few bounded reads - and `Segment(n)`
  then builds any single media segment by seeking straight to its window
  through the Cues and reading only that window. A server answers HLS
  requests (master/media playlist, init, any segment) in milliseconds with
  nothing pre-generated; with an `httpfs` source only the ranges a viewer
  actually watches are transferred. Every resource is byte-identical to what
  the full `to-hls` pass writes (regression-tested on synthetic and real
  files), so both serving modes mix transparently - including cover art and
  global tags in the init segment. `plan.Resource(ctx, name)` is the
  declarative entry point (player-facing name → bytes + Content-Type, an
  HTTP handler is one call; `Resources()` lists every servable name), and
  text subtitle tracks are declared in the master playlist and served as one
  whole-presentation WebVTT rendition each (lazy - one sequential pass on
  first request). The master `BANDWIDTH` is estimated from cluster sizes.
  New reader primitives back it: `WithCues()`, `WithTags()` and
  `WithAttachments()` keep those elements on the head-only path (the latter
  two through their SeekHead entries), `Container.SegmentStart` anchors the
  cue positions, and `NewBlockReaderAt` starts a block reader at a cued
  cluster. In the WebAssembly build the same engine is `openHLS(input)`: a
  handle `{resources, resource(name), segment(n)}` over a `Uint8Array` or a
  `Blob`/`File` - the latter read through ranged slices, so a huge local
  file plays through MSE with bounded memory (the browser demo does).
- **MP4/MOV packaging sources.** `to-hls`, `to-abr` and `hls-segment` (and
  the wasm `openHLS`) now accept MP4/MOV inputs, sniffed from the first
  bytes - no pre-remux step required. For the on-demand
  plan the moov sample table IS the index: the plan is exact by
  construction, so every resource - master playlist, DASH manifest and
  I-frame playlist included - is byte-identical to the full pass
  (regression-tested; decoder-verified for both manifests).
- **ABR light** (`mp4.RemuxToABR`, CLI `to-abr`). Packages several
  pre-encoded quality variants of the same content into one multi-variant
  HLS presentation without transcoding: the first source is the reference
  (its audio tracks and subtitles serve every variant), the others
  contribute their video rendition only (`Options.VideoOnly`, also exposed
  standalone). The top `master.m3u8` carries each variant's real
  `BANDWIDTH`/`RESOLUTION`/`CODECS` over the shared audio/subtitle groups;
  security options (AES-128, `RewriteURL`) apply to every variant.
  decoder-verified end to end on a two-quality set.
- **Single-file byte-range serving** (`Options.SingleFile`, CLI
  `--single-file`). Each rendition becomes one progressive file - init +
  `sidx` Segment Index + all CMAF fragments, byte-identical to the segmented
  mode's - served by ranges: `EXT-X-BYTERANGE` playlists and the DASH
  on-demand profile (`SegmentBase`/`indexRange`). Two media files instead of
  hundreds; the server only needs HTTP Range support. decoder-verified for
  both manifests. Incompatible with `Encrypt`.
- **Trick-play I-frame playlists.** `to-hls` emits `iframe.m3u8`
  (`EXT-X-I-FRAMES-ONLY`) declared in the master via
  `EXT-X-I-FRAME-STREAM-INF`: one keyframe per segment referenced as a byte
  range into the existing segments (styp + moof + mdat header + the
  keyframe sample) - zero extra media, decodability of a range verified
  end to end (range → decoder → JPEG). MP4-source on-demand plans expose it
  too (ranges computable head-only); not emitted when encrypting. DASH
  trick mode is not emitted: it requires a derived reduced track
  (transcoder territory).
- **Audio-only presentations.** Music/podcast sources (no video track)
  package fine: the first audio track is the primary rendition (historical
  file names), segment boundaries follow its sample grid, the master
  carries no RESOLUTION. decoder-verified for HLS and DASH.
- **HLS delivery security.** `Options.Encrypt` (`HLSEncryption{Key, KeyURI,
  IV?}`) AES-128-encrypts every media segment - whole-segment CBC with PKCS#7
  and IV = media sequence per RFC 8216 - and writes the `EXT-X-KEY` line;
  `to-hls`/`hls-segment` expose it as `--aes-key`/`--aes-key-uri`. Both
  serving modes produce identical ciphertext; init segments and subtitles
  stay clear; the key is only advertised, never stored. Verified by an
  openssl decrypt round-trip of the whole presentation (a mainstream HLS
  demuxer does not decrypt whole-segment fMP4; hls.js does). AES-128 is
  HLS-only, so no DASH manifest is emitted when encrypting. Alongside it,
  `Options.RewriteURL func(name) string` (CLI `--url-prefix`) rewrites every
  URI the playlists/MPD reference - the hook for CDN prefixes and per-URL
  signed tokens (HMAC example in library.md). On-demand subtitle cues are
  now loaded once and cached, and the windowed `subN_*.vtt` resources make
  the on-demand subtitle playlists byte-identical to the full pass.
- **Keyframe extraction for thumbnails/scrubbing**
  (`matroska.ExtractKeyframeSample`, CLI `extract-frame`). Returns the video
  keyframe nearest a timestamp, seeked through the Cues (a few bounded
  reads) and packed decoder-ready - Annex-B with the parameter sets
  prepended (H.264/HEVC) or an IVF wrapper (VP8/VP9/AV1). mkvgo never
  decodes: the image is one decoder call away; a scrubbing
  storyboard is a loop over the keyframe index. Verified end to end
  (extracted frame → decoder → JPEG).
- **Remote files over HTTP Range** (`httpfs` package, CLI URL support). The
  new `httpfs` package implements the FS port over ranged GETs (cached
  512 KiB windows, configurable client/headers, explicit refusal when a
  server ignores `Range` - never a silent full download; `BytesFetched()`
  reports the transfer cost). Combined with the head-only probe, indexing a
  remote media library transfers a few kilobytes per file. The CLI accepts
  `http(s)://` URLs on the inspection commands (`info`/`tracks`/`probe`/
  `keyframes`, plus `chapters`/`tags`/`attachments` for MP4) and as the
  source of `to-mp4`/`from-mp4`/`to-hls` via `httpfs.Hybrid()` (URLs read
  over HTTP, writes on the OS) - remuxing straight from a NAS/S3 URL is a
  streamed download. Verified byte-identical to a local remux.
- **Deterministic outputs, guaranteed.** The writers never stamp wall-clock
  times or random IDs (fixed `MuxingApp`/`WritingApp`, `DateUTC` only copied
  from the source, MP4 timestamps zero), so the same input and options
  produce byte-identical files - now documented as a guarantee and locked by
  a regression test (MKV rewrite, MP4 remux, HLS segments over the in-memory
  FS). Safe for content-addressed storage, dedup and golden tests. `make
  build`/`make release` now set `CGO_ENABLED=0` (pure-Go static binaries).
- **WebAssembly build** (`make wasm` → `dist/wasm/mkvgo.wasm`, ~4.7 MB raw /
  ~1.3 MB gzipped). The probe/remux/HLS engine runs client-side: a global
  `MkvGo` object exposes `probe`, `remuxToMP4`, `remuxFromMP4`, `remuxToWebM`,
  `remuxToHLS` and `extractSubtitleVTT`, all Promise-based, with the input
  format sniffed from its first bytes. `probe` also accepts a `Blob`/`File`
  read through ranged slices - head-only, so probing a 40 GB file in a file
  input transfers a few hundred kilobytes without uploading anything. A typed
  TypeScript wrapper (`web/mkvgo.ts`), a runnable browser demo with MSE
  playback of the HLS output (`web/example/`), React/Vue integration examples
  (`docs/wasm.md`) and a Node end-to-end smoke test (`make wasm-smoke`) ship
  with it.
- **WebAssembly ergonomics.** Every wasm method now honours
  `{ signal?: AbortSignal }` - aborting cancels the in-flight Go work (probe
  reads, remux, segment builds), wired for React effect cleanups.
  `hlsSegmentStream(plan)` exposes the video rendition as a progressive
  `ReadableStream`. `web/react.ts` ships copyable hooks: `useMkvGo`,
  `useProbe` (auto-abort), `useHLSPlayer` (MSE playback of a local File via
  on-demand demuxed segments, bounded memory).
- **Runnable examples.** `examples/hls-server/` is a complete ~90-line
  on-demand HLS + DASH server (`mp4.PlanHLS` + one handler, local file or
  http(s):// URL source, landing page that plays the output in hls.js and
  dash.js). `web/vue.ts` adds Vue 3 composables mirroring the React hooks
  (`web/react.ts`). Both TypeScript files typecheck under `--strict`.
- **`mkv.MemFS`** - an in-memory implementation of the `FS` port. Every
  operation taking `Options{FS: …}` can run without a filesystem (the wasm
  build's foundation, also useful for tests and servers assembling outputs in
  memory): `NewMemFS()`, `Put`/`Get`/`Paths`, and `FS()` returning the wired
  port.
- **`validate` streaming-readiness checks.** Beyond the structural checks,
  `validate` now audits what seeking and on-demand serving rely on: missing
  Cues index, cue points referencing a non-video track (error - seeking
  lands on audio; the write-side bug class fixed this release), cue times
  matching no actual video keyframe (stale index), subtitle blocks without
  BlockDuration, video tracks without DefaultDuration, AAC without its
  AudioSpecificConfig, attachments without a MIME type. Each finding names
  the fix.
- **Streaming non-goals documented.** LL-HLS (a live-ingest mechanism  - 
  mkvgo packages VOD files) and multi-period DASH (a discontinuity model
  that would degrade seeking if used for chapters) were evaluated and
  deliberately left out; the rationale lives in library.md.

## [0.12.0] - 2026-07-02

### Fixed

- **Block timing on non-default `TimecodeScale` sources.** `WriteCluster` and the
  `StreamWriter` converted the cluster timestamp to timecode-scale units but wrote
  each block's relative offset in raw milliseconds, so any source authored with a
  scale other than 1 ms produced silently mistimed blocks when its clusters were
  rebuilt (mux/merge/split/join/edit; `reindex` copies verbatim and was unaffected).
  Offsets are now converted like the cluster timestamp, and an offset that cannot
  fit a SimpleBlock's int16 is an explicit error instead of a silent int16 wrap.
  Sparse extra subtitle cues (merge-subtitle) now roll clusters forward instead of
  overflowing the current one; `WriteSegmentInfo` falls back to the Matroska default
  scale (1 ms) when `TimecodeScale` is 0 so the Duration element is still derived.
- **Split keyframe alignment gates on video keyframes.** Every audio block is
  flagged keyframe, so alignment on "any keyframe" opened a segment on audio and
  then admitted mid-GOP video (corrupt until the next real keyframe). A segment now
  starts at the first video keyframe at/after the range start (leading audio is
  dropped with it), keeps the GOP straddling the end cut (no frame lost across
  chained segments), and a range that contains media but no video keyframe is an
  explicit error instead of a silently empty or corrupt part.
- **Rebuilt Cues key on video keyframes.** `reindex` and the full-rewrite edits
  cued the first keyframe-flagged block of each cluster - and every audio block
  carries that flag, so mixed clusters cued audio and the real video keyframes
  were never indexed. The cue scan now keys on video tracks (audio-only files
  keep the throttled first-block cue).
- **Split rebases chapters.** Each segment now carries only the chapters
  overlapping its range, clipped and shifted to the segment timeline (previously
  every part carried the full source list at absolute timestamps).
- **`validate`/`compare` exit codes.** `validate` exits `1` when error-severity
  issues are found (warnings are printed but do not fail; `-strict` makes them
  fail too) and `compare` when the files differ - both previously always exited
  `0`, so neither could gate a script.
- **`Merge` no longer discards attachments.** `MuxOptions` gained
  `Title`/`Attachments`; `Merge` carries the first input's title, chapters, tags
  and attachments (first-wins, now documented). `MergeOptions.Progress` was dead  - 
  it is now honoured.
- **`RemoveTrack` drops orphan tags.** Tags targeting a removed track's UID are
  no longer carried into the output (they pointed at a track that no longer
  exists); global tags and tags on kept tracks survive.
- **`mp4.OpenMeta` godoc** claimed Tags stay nil; they are populated from the
  iTunes `ilst` atoms (and the title from `©nam`) since 0.11.

### Added

- **QuickTime `.mov` support (non-faststart).** The MP4 reader now parses the
  layout raw iPhone/camera QuickTime files use - `wide` + `mdat` first, `moov`
  at the end - which previously failed with `box ... has invalid size`: the
  audio SoundDescription version 1 (16 extra per-packet bytes before the
  extension boxes) and version 2 (64-byte struct, float64 sample rate) are now
  honoured, and an esds wrapped in a QuickTime `wave` extension is unwrapped.
  `OpenMeta` and `RemuxFromMP4` work on such files (real-muxer fixture added);
  output verified decodable by a real decoder.
- **The `mp4` package is now stable API**, with the same policy as the
  `matroska` facade: held additive and backward-compatible across 0.x
  releases. (It is fuzzed, decoder-verified end to end, and used in
  production.)
- **Self-verifying files.** `mkvgo hash` stores each track's content SHA-256
  as a `CONTENT_SHA256` tag (in place on mkvgo-written files, thanks to the
  metadata reserve); `mkvgo verify` recomputes and exits `1` on any mismatch  - 
  bit rot and transfer corruption are detectable with no external checksum
  file. MP4s are hashed at remux time (`to-mp4 --hash`, computed while the
  samples stream so it costs no extra I/O; stored as freeform `ilst` atoms)
  and `verify` reads them back from the sample table. Library:
  `matroska.WriteContentHashes`/`VerifyContentHashes`,
  `mp4.Options.ContentHashes`/`mp4.VerifyContentHashes`.
- **In-place edits that grow.** mkvgo-written files now reserve 1 KB of Void
  after the metadata (`writer.MetadataReserve`, mkvpropedit-style), so
  `edit-inplace` absorbs a longer title or added tags/chapters instantly
  instead of demanding a full rewrite. The edit also REBUILDS the head
  SeekHead in place (it used to be overwritten and lost, degrading head-only
  reads) keeping its Cues entry, and folds a post-cluster statistics Tags
  element into the head instead of duplicating it.
- **`compare -blocks`.** Content-level comparison: per-track block count,
  payload bytes and payload SHA-256 in stream order (`matroska.CompareBlocks`).
  An identical result proves a remux/reindex/split+join round-trip carried
  every frame byte-identically - beyond what the metadata compare shows.
- **`make e2e`** verifies the remux paths against a real external decoder/prober (local
  or `MKVGO_E2E=docker:<container>`): fixture generation, to-mp4/faststart,
  from-mp4, vp09, seekable WebM, QuickTime `.mov`, `split -every`, decode
  checks. New fuzzers cover the OGM chapter parser and the VP9 frame header
  (the OGM one immediately caught non-monotonic chapter times - now an
  explicit error), and the QuickTime fixture seeds the MP4 fuzzer.
- **`add-attachment` / `remove-attachment`.** Attach a file (font, cover art  - 
  MIME sniffed from magic bytes, `-mime` to override) or remove one by ID or
  exact name; removal fails before writing anything when nothing matches.
- **Chapters as OGM text.** `set-chapters` replaces a file's chapters from the
  `CHAPTER01=…`/`CHAPTER01NAME=…` format chapter-aware tools exchange;
  `extract-chapters` exports them (MP4/MOV accepted). Library:
  `matroska.ParseOGMChapters`/`FormatOGMChapters`.
- **`split -every <duration>`.** Splits into keyframe-aligned segments of
  roughly the given duration, boundaries taken from the Cues index. And
  `split -pattern` is exposed on the CLI; with `-chapters` the `{title}` token
  names each part after its (sanitized) chapter title.
- **Cross-format `compare`.** Either side of `mkvgo compare` may now be an
  MP4/MOV (read via the head-only MP4 probe), so a remux round-trip can be
  verified: `mkvgo compare movie.mkv movie.mp4`. The library gains
  `matroska.CompareContainers` for already-parsed containers.
- **Per-track statistics tags on mux.** `Mux`/`Merge` output
  now carries convention statistics tags - `BPS`, `DURATION`,
  `NUMBER_OF_FRAMES`, `NUMBER_OF_BYTES` per track, keyed by track UID  - 
  accumulated while streaming and written in a Tags element the SeekHead points
  to, so `matroska.WithBitrate()` (and the conventional `TAG:BPS`) read them head-only.
- **VP9 → MP4.** VP9 tracks remux to a `vp09` sample entry (VP9-in-ISOBMFF).
  The `vpcC` configuration record is taken from the Matroska CodecPrivate when
  one is present, and otherwise derived from the first keyframe's uncompressed
  header (profile, bit depth, chroma subsampling, colour range) plus the
  track's colour code points. `from-mp4` reads `vp09` back (the `vpcC` becomes
  the CodecPrivate, so colour survives). Output verified decodable by a real decoder.
- **Cover art across MKV ↔ MP4.** `to-mp4` carries the source's first JPEG/PNG
  image attachment (one named `cover.*` preferred) as the iTunes `covr` atom;
  `from-mp4`/`OpenMeta` bring a `covr` back as a Matroska attachment
  (`cover.jpg`/`cover.png`). Fonts and other non-image attachments are still
  not representable in MP4.
- **`split -range` accepts clock times.** Each range bound can be milliseconds
  (`300000`), fractional seconds (`90.5`) or `[HH:]MM:SS[.fraction]` (`5:00`,
  `01:30:00`).
- **Seekable WebM output.** `RemuxToWebM`/`to-webm` now writes a real indexed
  file - known-size clusters, a Cues seek index and a SeekHead - instead of the
  unknown-size streaming layout with no index. The output is readable by the
  seekable reader, `keyframes` works head-only on it, and players can seek.
  `Options.Progress` is honoured by the remux (it was ignored).
- **CLI overwrite guard.** Commands that write a new file refuse to overwrite an
  existing one unless the global `-f`/`--force` flag is passed. `edit-inplace`
  (which edits its input by design) is flagged as destructive in its help.
- **Progress everywhere.** `Options.Progress` is honoured by Mux, Merge, Split,
  Join, Demux and AddTrack (aggregated across sources for multi-input operations);
  the CLI shows a progress bar on all cluster-rewriting commands.
- **Runtime loss warnings.** `to-webm` warns about the chapters/attachments/tags
  actually present in the source and dropped by the WebM subset; `from-mp4` reports
  dropped tracks via `OnDrop` (parity with `to-mp4`).
- **`make fuzz`** runs the parser fuzzers locally; `README` gained consolidated
  *Limitations* and *Versioning* sections; command help now shows flag defaults.

## [0.11.0] - 2026-06-26

### Added

- **Sample-exact audio across MP4 <-> MKV round trips.** Audio that survives a
  `RemuxFromMP4` / `RemuxToMP4` round trip now decodes bit-identically to the source
  for AAC, FLAC, AC-3 and E-AC-3 (verified by decoding both sides to PCM). Opus and
  MP3 stay sample-accurate at the head (no desync) but may keep a few samples of
  encoder padding at the tail - their delay is intrinsic to the bitstream and decoders
  do not trim that tail from an MP4 either. Two changes make this hold:
  - New `Track.CodecDelay` / `Track.SeekPreRoll` (Matroska `0x56AA`/`0x56BB`, ns) are
    read and written. A codec's container-signalled encoder/decoder delay (the MP4
    edit-list `media_time`) is carried as `CodecDelay` on the MKV side and re-emitted
    as an MP4 edit list (`elst`) on the way back, for AAC/AC-3/E-AC-3. Opus, MP3 and
    Vorbis carry their delay intrinsically (Opus pre-skip in the OpusHead, MP3 in its
    in-band Xing/LAME header), which decoders apply regardless of the
    container, so they are excluded - a derived edit list would over-trim them;
    FLAC/DTS/PCM have no priming. (`Options.MP3ContainerDelay` /
    `--mp3-container-delay` opts MP3 back into the edit-list path, to round-trip an
    MP3 that originated in an MP4 - rare; it over-trims a native-MKV MP3, so it is off
    by default.)
  - **Audio tracks are written on a sample-rate media timescale** (`mdhd`), as mainstream muxers
    do, instead of the millisecond movie timescale. This makes the edit list's
    `media_time` sample-exact, which is required for a demuxer to trim a codec's priming
    precisely - notably AC-3, whose decoder delay it ignores from a millisecond-
    quantised edit list. Video/text/subtitle timing is unchanged.
- **Per-track bitrate from Matroska tags.** The `BPS` tag mainstream muxers write per
  track (bits per second) is now surfaced as the typed `Track.Bitrate`, keyed by the
  track UID. This is the value probers expose as `TAG:BPS` - they leave their own
  per-stream `bit_rate` field `N/A` for Matroska, so this gives more than an external prober
  there; for MP4 `Track.Bitrate` comes from `btrt`/`esds` and equals the conventional
  `bit_rate`. A full `Read` fills it; the metadata-only probe does too under the new
  `matroska.WithBitrate()` / `reader.WithBitrate()` option, which follows the head
  `SeekHead` straight to the `Tags` element (one seek, no Cluster scan - the muxer
  references `Tags` from the head). Off by default, so the default probe stays minimal.
- **Extended disposition flags.** `Track.HearingImpaired`, `VisualImpaired`,
  `TextDescriptions`, `Original` and `Commentary` expose the Matroska
  FlagHearingImpaired/…/FlagCommentary elements - the conventional stream dispositions
  of the same name - alongside the existing default/forced. Shown in `probe` and
  JSON. Matroska-only (MP4 has no equivalent boxes).
- **3D stereo + 360 projection.** `Track.StereoMode` (with `StereoModeName()`) and
  `Track.Projection` report stereoscopic-3D arrangement and spherical/360
  projection - from the Matroska StereoMode/Projection elements or the MP4
  `st3d`/`sv3d` boxes (st3d mapped to the Matroska StereoMode values). Shown in
  `probe` and JSON; unset for ordinary 2D video.
- **Average frame rate.** `Track.AvgFrameRate()` returns the conventional
  `avg_frame_rate` (frame count over duration) - non-zero for MP4 video
  (head-only), 0 for Matroska where the header carries no frame count. Surfaced in
  `probe` (when it diverges from the nominal rate, i.e. VFR) and JSON.
- **HDR10 static metadata.** `Track.HDR` (`HDRStaticMetadata`) now carries the
  Content Light Level (`MaxCLL`/`MaxFALL`, cd/m²) and the SMPTE ST 2086 Mastering
  Display colour volume (`MasteringDisplay`: R/G/B + white-point CIE 1931
  chromaticities and the luminance range), read head-only from the Matroska
  Colour element (`MaxCLL`/`MaxFALL` + `MasteringMetadata`) or the MP4 `clli`/
  `mdcv` boxes - the frame side data probers report, the last colour/HDR gap
  versus a head-only probe (HDR detection, CICP colour and Dolby Vision were
  already covered). mdcv's fixed-point values (and its G,B,R primary order) are
  normalised to the same units as the Matroska floats. Shown in `probe` output
  and JSON (`hdr`). nil when the stream carries no such metadata.

### Fixed

- **A/V presentation offset preserved across MP4.** A per-track start offset (the
  empty edit mainstream muxers write for a sync correction - e.g. audio starting
  476 ms after video) was read into the block timestamps but never re-emitted by
  `to-mp4`: each track was rebased to 0, silently desyncing the audio. `to-mp4` now
  writes the offset as a leading empty edit (`media_time -1`), so the A/V gap
  survives the round trip. Coded data was always intact; only the timing was lost.
- **Split no longer trims a frame of real audio at the seam.** Every output segment
  inherited the source's `CodecDelay` (encoder priming), but only the first segment
  actually starts with that priming - a later segment begins on real audio. A
  decoder or `to-mp4` then trimmed `CodecDelay` worth of real samples (one AAC frame,
  ~23 ms) at each cut. Later segments now drop `CodecDelay`, so a split is lossless:
  no coded packet is dropped and the decoded sample count is exact. (Decoding a later
  segment in isolation still differs from the source within the segment - AAC frames
  share overlap-add context across the cut, so a clean boundary is impossible without
  re-encoding; this is codec physics, not a loss. The coded stream is intact.)
- **Last chapter's end time preserved across MP4.** The Nero `chpl` box carries only
  start times, so `from-mp4` closed each chapter at the next one's start and left the
  last open (read back as `0`). The last chapter now closes at the movie end, so an
  explicit final `ChapterTimeEnd` survives the round trip.
- **Container title, global tags and track name now map to MP4.** `to-mp4` carried a
  track's language but dropped the container title, the other global tags and the
  per-track name. All are now written where the tooling reads them: the title as
  `moov/udta/meta/ilst/©nam` (the conventional format `title`); the other global tags as
  their iTunes atoms (`ARTIST`→`©ART`, `ALBUM`→`©alb`, `DATE_RELEASED`→`©day`,
  `GENRE`→`©gen`, `COMMENT`→`©cmt`, `COMPOSER`→`©wrt`, `DESCRIPTION`→`desc`,
  `ENCODER`→`©too`); and the track name as the `hdlr` name (the conventional stream
  `handler_name`, the only per-track string MP4 exposes - MP4 has no readable stream
  `title`) plus the QuickTime `trak/udta/name` box mainstream muxers write. `from-mp4` reads
  them all back into `Info.Title` / `Tags` / `Track.Name`, so they survive a round
  trip (and an MP4-in/MP4-out edit) instead of being lost at the MP4 boundary.
- **MP4 sample-table complexity DoS.** A constant-size `stsz` (and the keyframe
  index's frame count) was bounded only by the 134M `maxSamples` cap, so a tiny
  box could declare a huge sample count and force a multi-second, multi-GB
  allocation on a small file. The count is now bounded by the file size (samples
  must physically fit) before any allocation, on both the full-table and
  keyframe-only paths.
- **MP4 chunk/stsc quadratic.** Building the full sample table looked up the
  samples-per-chunk with a linear scan of the stsc entries *per chunk* - O(chunks ×
  entries), which a forged stco+stsc pair could turn into a multi-second stall. A
  monotonic cursor makes it linear (O(chunks + entries)). Sources were checked for matching track count and
  type but not codec, so concatenating mismatched codecs (e.g. H.264 + HEVC)
  silently produced a broken file. Join now also requires matching `Codec` and
  codec-private configuration per track.
- **`Join` A/V drift.** Each input was offset by a single per-file value (the
  container duration), which drifts when tracks end at slightly different times.
  Each track is now rebased on its own end, so audio and video stay in sync across
  joins.
- **`Join` redundant opens.** Each source was opened three times (validation,
  duration, streaming); the metadata is now read once and cached, halving the open
  count - relevant for an S3/HTTP `FS`.
- **Fixed-lacing residual bytes.** A fixed-lacing block whose data size is not a
  multiple of its frame count now errors instead of silently dropping the
  remainder.
- **Output Close errors surfaced.** `RemoveTrack`, `AddTrack`, `EditMetadata`,
  `Join`, `Split`, `Reindex`, `MergeSubtitle`/`MergeASS`, `ExtractSubtitle`/
  `ExtractASS` and `RemuxToWebM` now return a Close error on the success path
  (e.g. a custom `FS` that commits the write on Close), matching `Mux`.
- **Duplicate Info/Tracks (non-conformant files).** A file with more than one
  Info or Tracks element (the spec allows one each) is now handled "first wins"
  by both `Read` and `ReadMeta` - previously the full `Read` appended a second
  Tracks set, doubling the tracks, and the two readers could disagree.
- **Debuggable parse errors.** A parse failure now carries the failing element's
  ID and byte offset (`element 0x… at offset N: …`) instead of a bare error, so a
  failure on a real-world file points at where it went wrong.

### Hardened

- **CLI rejects unknown flags.** A mistyped or unsupported `-`/`--` option (e.g.
  `to-mp4 --fastart`) was silently treated as a positional argument - a confusing
  `no such file` for some commands, silently ignored for others (`split`). Every
  command now fails fast with `unknown flag: …`. Flag values that contain a dash
  (e.g. `split -range 0-2000,2000-0`) are unaffected.
- **Context cancellation in inner parse loops.** `parseInfo`/`parseTracks`/
  `parseTrackEntry`/`parseTags` now honour a cancelled context, not only the
  top-level Segment walk, so a forged file with millions of sub-elements stops
  promptly on cancel.
- **Leaf-element framing.** Skipping an element now rejects an unknown size (-1)
  rather than seeking a byte backwards and desyncing the framing inside a leaf
  parser (only Segment/Cluster may legitimately be unknown-size).
- **Cumulative budget on head strings.** The Info app/title/segment-UID and track
  Name reads now count against the cumulative metadata budget instead of only the
  512 MB-per-element cap, so many large forged strings cannot exceed it in total.
- **Streaming reader memory budget.** `ReadStream` now enforces the same
  cumulative in-memory metadata budget as the seekable reader (codec-private,
  attachments, binary tags), so a forged stream cannot exhaust memory.

### CI

- **Nightly fuzzing.** A scheduled workflow runs every `Fuzz*` target for a
  bounded time, exploring fresh inputs beyond the committed seed corpus. The
  coverage corpus is cached between runs (`$GOCACHE/fuzz`), so each night resumes
  from the previous night's findings and the search deepens over time rather than
  restarting cold.
- **Complexity guard in the MP4 fuzzer.** `FuzzParseMP4` now times each input and
  fails if a single parse exceeds a deadline - Go fuzzing flags panics but not
  slow-but-completing inputs, so a complexity DoS would otherwise pass silently.

### Documentation

- **Approachable README + recipes.** The README now opens with what mkvgo is, a
  supported-formats table (MKV/WebM/MP4), a "why", and a real `probe` example
  before the reference. A new task-oriented cookbook ([docs/recipes.md](docs/recipes.md))
  shows common jobs with the CLI and the Go library side by side, and documents
  that the `-json` output equals `json.Marshal` of the library container.

## [0.10.0] - 2026-06-25

### Added

- **Colour determinacy signal.** A new `Track.ColourDetermined` reports that the
  colour was actually read from a source - the container Colour element, an MP4
  colr box, or the codec bitstream's colour signalling (H.264/HEVC VUI, AV1
  color_config, VP9 vpcC) - even when it resolves to "unspecified" (every Color*
  left nil). It lets a caller tell a confirmed-SDR/unspecified stream (true, no
  colour values) from one whose colour could not be read at all (false), rather
  than conflating both as a bare nil. Shown in `probe` output.
- **Complete keyframe index for Cues-less Matroska.** `WithKeyframeIndex()`
  (re-exported as `matroska.WithKeyframeIndex`) builds the *complete* video
  keyframe index - every keyframe from a structural scan, not a
  sample - for a Matroska that carries no Cues. After the head parse, and only
  when no Cues were found, it makes a single sequential read-ahead pass over the
  Segment (cluster by cluster, never a per-block seek), reading element headers
  and discarding block payloads in-stream - no demux, no decode. It transfers the
  Segment like an external prober but pure-Go in-process; discarding rather than seeking past
  payloads keeps the reads sequential, so it stays I/O-bound (not seek-bound) on a
  high-latency SMB/NAS mount. Per
  Cluster it reads the Timestamp, then each SimpleBlock/BlockGroup, emitting a
  keyframe (SimpleBlock keyframe flag, or a BlockGroup with no ReferenceBlock) on
  the video track only at `Timestamp + relative-timecode`. It is the "no external
  fallback" path; `WithSampledKeyframes` remains the cheaper coarse variant. Files
  with Cues are never scanned. The CLI `keyframes` command uses it for such files.
  (Matroska has no edit list, so no time shift is applied - parity with the
  Cues-derived index.)
- **Sampled keyframe index for Cues-less Matroska.** `WithSampledKeyframes(n)`
  (re-exported as `matroska.WithSampledKeyframes`) recovers a coarse keyframe
  index for a Matroska that carries no Cues: after the head parse, and only when
  no Cues were found, it probes n evenly-spaced byte offsets in the Segment body,
  resyncing to the next Cluster at each. Within the Cluster it reads block headers
  (payloads skipped by element size) to find the first real video keyframe - a
  SimpleBlock keyframe flag, or a BlockGroup with no ReferenceBlock - and emits
  that block's exact presentation time, so every point is a genuine seek point
  even when the muxer does not align Clusters to keyframes (the Cluster start is
  NOT assumed to be a keyframe). A Cluster with no video keyframe is skipped
  (bounded). Bounded to about n seeks, no block-by-block decode, so a Cues-less
  file reports keyframes instead of forcing an external fallback. The CLI
  `keyframes` command uses it automatically for such files.
- **Fragmented-MP4 metadata.** For a fragmented MP4 (an mvex box in the moov),
  the probe now recovers the frame rate from the fragment defaults (mvex>trex)
  and the keyframe index from a random-access index - the mfra/tfra at the file
  tail, or the sidx Segment Index at the head that streaming fMP4 carries - both
  bounded and head-only, so fragmented files report a frame rate and (closed-GOP)
  keyframes without a full demux. `Container.Fragmented` flags the file (also in
  `probe` output and JSON); when no such index is present those fields stay unset
  and the caller should fall back.

### Performance

- **Cheap MP4 keyframe index.** The MP4 keyframe index (`OpenMeta` with
  `Keyframes`, CLI `probe`) is now derived from the sync-sample table
  (stss + stts/ctts) without building the full sample-offset table, so it costs a
  fraction of the CPU on a long movie while reporting identical keyframes. The
  full table is still built for remux/extract.
- **Lazy moov read for the metadata probe.** `OpenMeta` / `ReadMeta` read only
  the moov boxes the metadata and keyframe index need, seeking over the large
  sample-table bodies (stsz/stco/co64/stsc) instead of reading them; reads are
  coalesced, so it is both I/O- and round-trip-light - about 90% fewer bytes read
  and ~2× faster on a high-latency (9p/SMB/NAS) mount, with lower variance. The
  full table is still read for remux/extract; any unexpected layout falls back to
  the full read, and the result is byte-identical.

### Fixed

- **Truncated Matroska tail.** A file cut mid-element after the head metadata
  (Info + Tracks) now returns what was parsed instead of failing with an
  unexpected EOF, as external probers do on a truncated file.
- **Desynced MP4 box walk.** When the top-level box walk runs into the mdat (a
  file with a wrong mdat size), `findMoov` falls back to a bounded, validated
  backward scan for the moov, recovering files mainstream probers still read.
- **Typed non-Matroska error.** `Open`/`Read`/`OpenMeta`/`ReadMeta` return the new
  `ErrNotMatroska` (matchable with `errors.Is`) when a misnamed `.mkv` is actually
  an MP4-family file, so a caller dispatching by extension can re-route to the mp4
  reader instead of getting a cryptic EBML error.

## [0.9.1] - 2026-06-25

### Added

- **In-band colour fallback (opt-in).** `reader.WithInBandColourFallback()`
  (re-exported as `matroska.WithInBandColourFallback()`) and
  `mp4.Options{InBandColour: true}` recover a video track's colour when it is
  carried only in an in-band HEVC SPS - a bare hvcC with no parameter sets and no
  container colour, as some streaming-style HDR muxes write. The head parse is
  unchanged; only a track that still lacks colour reads a bounded slice of its
  first sample, parses the SPS VUI, and applies an Alternative Transfer
  Characteristics SEI (payload type 147) override - HLG's `bt2020-10` →
  `arib-std-b67` compatibility signal. Off by default; tracks that carry colour
  in the header read no frame.

### Performance

- **Full read on high-latency filesystems (9p/SMB/network mounts).** A full
  `Read` / `matroska.Open` no longer walks every cluster header to reach the
  metadata that follows the media:
  - the Cues index is read in one bulk read instead of ~4 tiny reads per
    CuePoint;
  - when a SeekHead is present, the whole cluster region is skipped in a single
    seek (or the read stops at the first cluster when the SeekHead is already
    exhausted);
  - with no SeekHead, a trailing Cues index is located by a bounded scan back
    from EOF rather than a forward cluster walk;
  - the remaining cluster-walk fallback reads raw, with no buffer amplification.

  Net: reads that previously took seconds over a network mount complete in a
  handful of seeks, and the result is byte-identical to the unoptimised walk.

### Fixed

- **Windows.** The `mux` / `add-track` `file:trackID` spec is split on the last
  colon, so a drive-letter path (`C:\dir\file.mkv:1`) parses correctly; a test
  helper that left a temp file open now closes it so Windows can remove it.

### Build

- golangci-lint upgraded to v2 (go1.26 compatibility); the Go toolchain is pinned
  to 1.26.4.

## [0.9.0] - 2026-06-23

Field-equivalence pass: the metadata probe reports more of the standard prober stream fields,
validated against a reference prober over a sweep of real files.

### Added

- **Audio output sample rate for SBR (HE-AAC).** `Track.OutputSampleRate` carries
  the decoder's doubled rate (Matroska `OutputSamplingFrequency` 0x78B5, or the
  AAC `AudioSpecificConfig` SBR extension rate - explicit AOT 5 and the
  backward-compatible 0x2b7 sync extension), and `Track.EffectiveSampleRate()`
  returns what probers report as `sample_rate`. Read on both the MP4 and Matroska
  paths; the writer persists 0x78B5 for round-trip.
- **Codec level.** `Track.Level` exposes the SPS `level_idc` probers report as
  `level` (H.264, HEVC, AV1), via the shared codec-bitstream fallback.
- **Display aspect ratio for anamorphic video.** `Track.DisplayWidth` /
  `DisplayHeight` (their ratio is the display aspect), with `DisplayAspectRatio()`
  / `SampleAspectRatio()` helpers returning the conventional `display_aspect_ratio` /
  `sample_aspect_ratio`. Read, in precedence order, from the MP4 `pasp` box, the
  Matroska `DisplayWidth`/`DisplayHeight` (0x54B0/0x54BA) elements, or the H.264/
  HEVC SPS VUI `aspect_ratio_info` (Table E-1 `aspect_ratio_idc`, or Extended_SAR
  `sar_width:sar_height`) - the most common H.264 SAR carrier, read head-only from
  the avcC. The ratio is stored exactly (no rounding that would collapse a fine
  pixel aspect), and the helpers reduce it with the same bounded `av_reduce`
  (1024×1024) mainstream probers use, so the `sar`/`dar` strings match theirs on every
  ordinary ratio and tame absurd ones. Both reader paths and the writer handle the
  elements; the MP4 remux emits a `pasp` box so anamorphic display survives a
  round trip.
- **Per-stream average bitrate.** `Track.Bitrate` from the MP4 `btrt` box (or the
  esds `avgBitrate` for AAC).
- **Pixel format.** `Track.PixelFormat` (the conventional `pix_fmt`, e.g. `yuv420p`,
  `yuv420p10le`) composed from the codec's chroma subsampling and bit depth
  (H.264/HEVC SPS, AV1 colour config, VP9 `vpcC`). For HEVC `hev1` with in-band
  parameter sets, the 4:2:0 chroma of Main/Main 10 is taken from the `hvcC` profile,
  so it still reads head-only.
- **Field order.** `Track.FieldOrder` ("progressive"/"interlaced", the conventional
  `field_order`) from the Matroska `FlagInterlaced` (0x9A) element or the H.264
  `frame_mbs_only_flag`.
- **Frame count.** `Track.FrameCount` (the conventional `nb_frames`) from the MP4 `stsz`/
  `stz2` sample count - read head-only, no sample-table expansion. Matroska has no
  head-only frame count, so it stays 0 there.
- **Per-track duration.** `Track.DurationMs` (the conventional per-stream `duration`) from
  the MP4 `mdhd` (duration ÷ media timescale), so a track that differs from the
  movie length is reported individually. Matroska carries no per-track header
  duration, so it stays 0 there.
- **MP4 file-level tags.** The metadata probe now reads the iTunes/QuickTime
  `udta`/`meta`/`ilst` atoms - `Container.Tags` gets the text tags (`©nam`→`TITLE`,
  `©ART`→`ARTIST`, `©day`→`DATE_RELEASED`, `©too`→`ENCODER`, …) and `Info.Title` is
  filled from `©nam`, matching how the Matroska reader exposes tags. Non-text atoms
  (cover art) are skipped.
- **Codec long name and channel layout.** `Track.CodecLongName()` returns the conventional
  `codec_long_name` (e.g. "H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10"), and
  `Track.ChannelLayout()` returns `channel_layout` ("stereo", "5.1(side)", …) from
  the channel count - display strings the fast path previously omitted.
- **MP4 frame rate is read head-only.** `Track.FrameRate` is now derived from the
  `stts` header (media timescale ÷ first `sample_delta`) - the conventional `r_frame_rate`
  for constant-frame-rate video - so the metadata probe reports it without
  expanding the sample table (it previously needed `Options{Keyframes: true}`).
- **Display rotation.** `Track.Rotation` (0/90/180/270, clockwise) read from the
  MP4 `tkhd` display matrix - the same matrix probers expose as Display Matrix
  side data. Lets a player show portrait phone video the right way up.
- **CLI** - `probe` prints all of the above per track (codec long name, profile/
  level, pixel format, aspect ratio, rotation, frame rate, frame count, per-track
  duration, bitrate, field order, channel layout, output sample rate); `probe -json`
  and `tracks -json` carry the same fields, including the derived `codec_long_name`
  and `channel_layout`. See [docs/library.md](docs/library.md) for the full table.

### Fixed

- **HE-AACv2 Parametric Stereo channel count.** A PS stream codes a mono core
  (`channelConfiguration` 1) the decoder upmixes to stereo; `mp4.OpenMeta` now
  reports 2 channels like external probers, detecting both explicit (AOT 29) and
  backward-compatible (0x2b7 / 0x548) signalling.
- **QuickTime `nclc` colr type.** The MP4 probe read only the `nclx` colr type and
  silently skipped the QuickTime `nclc` form, leaving `color_space` empty on files
  that carry their matrix only there. Both types are now read, each CICP field
  taken independently (a matrix-only stream still reports its `color_space`).

### Notes

- Documented three accepted head-only limitations - data present only in the media
  frames, not in any header the probe parses, so external probers surface it by decoding a
  frame while mkvgo reports the header values: implicit in-band SBR and Parametric
  Stereo (audio), and colour signalled only in an in-band SPS (video).
- On pathological **near-square** sample aspect ratios, an external prober's `sample_aspect_ratio`
  can differ from mkvgo's: they re-derive the SAR from the dimension-reduced DAR
  (so the same VUI SAR prints differently at different resolutions), whereas mkvgo
  reports the exact VUI/`pasp` ratio. Display-only and imperceptible (all ≈ the same
  picture shape); mkvgo keeps the true signal rather than mirror that quirk.

### Tests

- **Greatly expanded the test surface**, driven by statement coverage and gremlins
  mutation testing. Statement coverage is now **≥ 90% in every package**
  (`cmd/mkvgo/commands` 16.6% → 97.9%, `mkv/reader` → 90.6%, `mkv/ops` → 92.0%,
  `mp4` → 92.9%; `ebml`/`mkv`/`mkv/subtitle`/`mkv/writer` 90-98%), and global
  mutation efficacy rose from 70% to 82% (mutator coverage 85% → 95%). The new
  tests cover the previously-untested error/edge paths: malformed and truncated
  inputs, bit-reader / Exp-Golomb boundaries, sample-table and EBML element/VINT
  parsing, the `av_reduce` aspect algorithm (brute-force property test), and the
  codec-bitstream skip paths (scaling lists, RPS).
- **CLI is now integration-tested.** Every command is exercised on a fixture
  (inspection via captured stdout and `-json`, edit/extract by re-reading the
  output, assembly/split/remux/reindex by parsing the produced file). To make the
  `Fatal`/error paths reachable in-process, the CLI's process exit is now an
  injectable hook (`Fatal` calls a `var osExit = os.Exit`) - behaviour is identical
  in production.

## [0.8.0] - 2026-06-23

### Added

- **MP4 metadata probe.** `mp4.OpenMeta` / `OpenMetaWithFS` / `ReadMeta` read an
  MP4's stream metadata head-only, reporting per track: **language** (`mdhd`
  ISO 639-2 - including QuickTime Macintosh language codes - and the `elng` BCP-47
  box), the **default** flag (`tkhd` track_enabled), the **forced** flag (DASH-role
  `kind` box), the **audio channel count** (from the AAC `AudioSpecificConfig` and
  AC-3/E-AC-3 `dac3`/`dec3`, not the unreliable `AudioSampleEntry` field), and
  **colour** code points (`colr`, falling back to the codec bitstream via the now
  exported `reader.FillColourFromCodecPrivate`).
- **Dropped (non-carried) tracks** are surfaced: the probe returns an additional
  `[]DroppedTrack` (cover art / attached pictures, hint/timecode/metadata tracks),
  each with its track ID, fourcc and reason. `RemuxFromMP4` reports them through
  `Options.OnDrop`. (A QuickTime chapter track is not reported - see Fixed.)
- **Dolby Vision.** `Track.DolbyVision` exposes the decoded
  `DOVIDecoderConfigurationRecord` (profile, level, RPU/EL/BL, `bl_signal_compatibility_id`),
  read from the MP4 `dvcC`/`dvvC` box (and the `dvhe`/`dvh1`/`dvav`/`dva1`/`dav1`
  sample entry types) or the Matroska `dvcC`/`dvvC` `BlockAdditionMapping`. It is
  carried through both remux directions, choosing the Dolby sample entry type
  (`dvh1`/`dva1`/`dav1`) for a non-cross-compatible stream and the plain
  `hvc1`/`avc1`/`av01` tag for a cross-compatible one. New `mkv.DolbyVision`,
  `ParseDolbyVisionConfig`, `EncodeDolbyVisionConfig`, `DolbyVision.BoxType`.
- **Head-only keyframe index** on `Container.Keyframes` ([]int64 ms, ascending,
  de-duplicated) - the cut points for `-c copy` HLS/DASH segmentation. MKV/WebM
  fills it from the `Cues` index via the `SeekHead` (one seek, no `Cluster` scan)
  in the metadata pass; MP4 fills it from the `stss`/`stts`/`ctts` tables, with the
  edit list (`elst`) applied as mainstream demuxers do, opt-in via `Options{Keyframes: true}`.
- **Subtitle extraction to WebVTT**, replacing an external conversion fork:
  `ops.ExtractSubtitleWebVTT` (embedded MKV/WebM track), `mp4.ExtractSubtitleWebVTT`
  (embedded MP4 tx3g/wvtt track) and `subtitle.FileToWebVTT` (external
  `.srt`/`.ass`/`.ssa`/`.vtt` sidecar), streaming to any `io.Writer`. Building
  blocks: `subtitle.Cue`, `WriteWebVTT`, `FormatVTTTime`, `SRTToCues`, `ASSToCues`,
  `FlattenASSBlock`, `ResolveCueEnds`.
- **Remux preserves what the probe reads.** `RemuxToMP4` writes the default flag
  (`tkhd` track_enabled), the forced flag (`kind` box) and the BCP-47 language
  (`elng`); the MKV writer persists `LanguageBCP47`. The edit list is folded into
  the composition times so `RemuxFromMP4` and the keyframe index present the de-facto muxer's
  timeline. MP4 video tracks report an average **frame rate** from the sample timing.
- **CLI parity.** `info`/`tracks`/`chapters`/`probe` accept an MP4/MOV path;
  `probe` prints colour, Dolby Vision, the keyframe index and dropped tracks; new
  **`keyframes`** command; `extract-subtitle` gains `-format vtt` (and an MP4
  source); new **`to-vtt`** command for external sidecars.

### Changed

- **MP4 `OpenMeta`/`ReadMeta` are head-only by default**: they read only the `moov`
  box headers and no longer expand the per-sample tables, so probing a long movie
  is far faster (the duration comes from `mvhd`). Pass `Options{Keyframes: true}` to
  build the sample table and populate `Container.Keyframes`. Both gain a variadic
  `...Options` parameter (existing two-arg calls are unaffected).
- **BREAKING:** `mp4.OpenMeta`, `OpenMetaWithFS` and `ReadMeta` now return
  `(*mkv.Container, []DroppedTrack, error)` (was `(*mkv.Container, error)`).

### Fixed

- The MP4 probe no longer reports a QuickTime **chapter track** (referenced via
  `tref/chap`) as a dropped track - its content is already read from `chpl`.

## [0.7.2] - 2026-06-22

### Added

- **WebVTT subtitles in MP4 remux.** `RemuxToMP4` now carries WebVTT tracks
  (`S_TEXT/WEBVTT` and the WebM-era `D_WEBVTT/*` ids legacy muxers write) instead
  of dropping them:
  - By default a WebVTT track is carried as `tx3g` timed text - the only MP4
    subtitle form read universally.
  - **`Options.NativeWebVTT`** (CLI `--webvtt-native`) carries it losslessly as
    native `wvtt` (ISO/IEC 14496-30) instead - cue settings and inline markup are
    preserved and Apple/Safari/CMAF read it, but many mainstream demuxers do not, so
    it is opt-in. `RemuxFromMP4` reads `wvtt` back to `S_TEXT/WEBVTT`.
- **`Options.FlattenStyledSubs`** (CLI `--flatten-subs`) carries ASS/SSA - which
  have no native MP4 form - as `tx3g`, stripping the dialogue framing and override
  tags (`{\...}`, `\N`, `\h`). Lossy: all styling/positioning/karaoke is discarded.
- Subtitles **never fail a remux**: a subtitle whose format cannot be carried is
  now always dropped with a reason via `Options.OnDrop` (pointing at the relevant
  flag), instead of being silently skipped or - for some inputs - aborting.
- New codec short name **`webvtt`** for `S_TEXT/WEBVTT` (reader and writer), for
  parity with `srt`/`ass`/`ssa`.

## [0.7.1] - 2026-06-21

### Added

- **`mp4.OpenMeta`** / **`OpenMetaWithFS`** / **`ReadMeta`** - metadata-only probe
  of an MP4 file, the counterpart of the MKV reader's `OpenMeta`/`ReadMeta`. They
  parse only the movie header (`moov`: track sample entries, colour code points,
  chapters) and return a `*mkv.Container` (Info, Tracks, Chapters, DurationMs)
  **without reading any sample data (`mdat`) or writing an output file**. This is
  the fast path for indexing/scanning an MP4 library - previously the only way to
  read an MP4's codecs/colour/chapters was a full `RemuxFromMP4` to disk.
  `RemuxFromMP4` and the probe now build their Matroska metadata from a single
  shared helper, so they report identical tracks/chapters/duration.

## [0.7.0] - 2026-06-21

New `mp4` package: remux between Matroska/WebM and MP4 (ISO base media file
format) without transcoding. It is isolated from the EBML core (shares no
low-level code with `ebml`/`mkv`) and is experimental.

### Added

- **`mp4.RemuxToMP4`** - MKV/WebM → progressive MP4. Compressed samples are
  copied verbatim into MP4 sample tables.
  - Video: H.264, HEVC, AV1. Audio: AAC, Opus, AC-3, E-AC-3, FLAC, MP3, DTS
    (incl. DTS-HD, carried as `mp4a`/`esds`). Subtitles: SRT (`S_TEXT/UTF8`) →
    `tx3g` timed text.
  - B-frame reordering preserved via a signed `ctts` box; colour/HDR code points
    written as a `colr` (nclx) box; 64-bit `mdat` with `co64` when needed.
  - Chapters written both as a Nero `chpl` box and a QuickTime chapter track
    (`tref`/`chap`).
  - `Options.FastStart` places `moov` before `mdat` for progressive HTTP
    playback; `Options.SkipUnsupported` drops tracks whose codec cannot be
    carried (reported via `Options.OnDrop`) instead of failing.
- **`mp4.RemuxFromMP4`** - MP4 → MKV. Reads the codecs above plus their MP4
  sample entries; colour and chapters round-trip back to the Matroska `Colour`
  element and chapter atoms.
- **`writer.WriteBlockGroup`** - writes a BlockGroup with a BlockDuration; used
  for subtitle cues. `WriteCluster` now emits a BlockGroup for any block with a
  non-zero `Duration`.
- **`Block.Duration`** - new additive field, populated from a BlockGroup's
  BlockDuration by the block reader.
- The MKV writer now emits the `Colour` element for tracks carrying colour code
  points.
- **CLI** - `mkvgo to-mp4 [--faststart] [--skip-unsupported] <in> <out.mp4>`,
  `mkvgo from-mp4 <in.mp4> <out.mkv>`, and `mkvgo to-webm <in> <out.webm>`
  (the latter exposing the existing `RemuxToWebM` on the command line).

## [0.6.0] - 2026-06-03

`ReadMeta`/`Read` now derive colour/HDR metadata from the codec bitstream when the
container Colour element (0x55B0) is absent. Many files signal colour only in the
codec SPS/VUI; such a track previously read as having no colour. Additive - no API
change, the same `Track` fields are populated more often.

### Added

- **Colour from the codec bitstream**, as a fallback to the container Colour
  element. When the container did not supply a field, it is filled from the
  in-memory `Track.CodecPrivate` (no extra file I/O):
  - **H.264** (avcC → SPS VUI): colour primaries / transfer / matrix, bit depth, profile.
  - **HEVC** (hvcC header + SPS VUI): bit depth and profile from hvcC; primaries /
    transfer / matrix from the SPS VUI.
  - **AV1** (av1C header + sequence-header OBU `color_config`): primaries /
    transfer / matrix, bit depth, profile.
  - **VP9** (vpcC fixed fields, when a CodecPrivate is present) - best-effort.

  The recovered values are CICP / ITU-T H.273 code points feeding the existing
  `ColorSpace`/`ColorTransfer`/`ColorPrimaries`/`ColorRange`/`VideoBitDepth` fields
  and `IsHDR()`. A Colour-less HDR10 track then reports `ColorSpaceName()="bt2020nc"`,
  `ColorTransferName()="smpte2084"`, `VideoBitDepth=10`, `IsHDR()=true` instead of
  empty/SDR.
- New additive field **`Track.Profile`** (e.g. "Main 10"), derived from the SPS.

### Behaviour

- The container Colour element stays **authoritative**: the bitstream only fills
  fields the container left nil (per-field precedence).
- CICP code 2 ("unspecified") from the bitstream is treated as absent (left nil);
  bit depth is constrained to {8, 10, 12}.

### Security

- The SPS / VUI / OBU parsing is **fail-soft**: a truncated, malformed or
  adversarial `CodecPrivate` never errors, panics, hangs or allocates unboundedly
  - the colour fields stay nil and the read continues. The parsers are panic- and
  hang-free on their own - a bounds-checked Exp-Golomb reader with a capped
  leading-zero run, every bitstream-driven loop count and bit width bounded, and
  emulation-prevention stripping; the `recover()` in the dispatcher is only a
  last-resort backstop. **`FuzzCodecColour`** drives random bytes straight at the
  parsers *without* that backstop, so a missing bound surfaces instead of being
  masked - it found and fixed an out-of-range bit depth and an Exp-Golomb-driven
  loop, both kept as regression seeds.

### Codecs covered

H.264, HEVC and AV1 are covered with hermetic byte-fixture tests. VP9 (vpcC) is
best-effort: VP9 colour usually lives in the container or in per-frame headers,
the latter outside the metadata path. VVC / Dolby Vision are out of scope.

## [0.5.0] - 2026-06-03

Fast metadata-only read path for library indexing. Additive - `Read` / `Open`
are unchanged.

### Added

- **`ReadMeta(ctx, r, path)`** plus **`OpenMeta`** / **`OpenMetaWithFS`**,
  mirroring the `Read` / `Open` / `OpenWithFS` trio (also re-exported from the
  `matroska` facade). They return the same `Tracks` + `Info` (and `DurationMs`)
  as a full `Read` - byte-identical, via the same `parseInfo` / `parseTracks`
  logic - but stop as soon as both are parsed:
  - never parse the Cues index, never traverse Clusters;
  - reads are buffered (~2 KiB) so the byte-at-a-time EBML reads cost one syscall
    instead of hundreds (matters on a network-mounted library);
  - a head `SeekHead` is used to jump straight to Info/Tracks, so a file whose
    `Tracks` element sits after the first Cluster still works without scanning.
  - `Chapters`, `Attachments`, `Tags` and `Cues` are left **nil** - call
    `Read` / `Open` for those.
  - Hardened for untrusted input: a forged `SeekHead` cannot make the fast path
    over-read (the `SeekID` size is bounded to a real element-ID width and
    `SeekPosition` offsets are range-checked).

### Performance (measured)

On 5 real 5-9 GB muxer-written files (`bench/main.go`), per file:

| read                | bytes read | time        |
|---------------------|-----------:|------------:|
| `reader.Read` (full)|   ~180 KB  | ~17,000 ms  |
| `ReadMeta`          |    ~2 KB   |    ~0.2 ms  |

`ReadMeta` reads ~90× fewer bytes and is ~80,000× faster than the full `Read`,
The full `Read`'s
cost is the Cues index (~790 KB across the five files) plus walking every
Cluster - neither needed for indexing. A media server can now use the in-process
reader for indexing instead of forking an external prober per file.

## [0.4.0] - 2026-06-03

Probe metadata: the track reader now exposes the fields a media indexer needs to
match standard prober output, and can distinguish "explicitly set in the file"
from "spec default". All struct changes are additive - existing exported fields
and types are unchanged.

### Added

- **Language**
  - `Track.LanguageBCP47` - the IETF BCP-47 language element (`0x22B59D`), now
    parsed alongside the legacy ISO 639-2 `Language` (`0x22B59C`). Modern muxers
    that write only BCP-47 are no longer mis-read.
  - `Track.ResolvedLanguage()` - effective language with BCP-47 taking precedence
    over the legacy element, per the Matroska spec.
- **Presence flags** - tell an explicit value from an applied default:
  `Track.LanguagePresent`, `Track.DefaultPresent`, `Track.ForcedPresent`.
- **Video colour** - parsed from the `Colour` element (`0x55B0`):
  `Track.ColorSpace` (MatrixCoefficients `0x55B1`), `Track.ColorTransfer`
  (`0x55BA`), `Track.ColorPrimaries` (`0x55BB`), `Track.ColorRange` (`0x55B9`) as
  raw CICP / ITU-T H.273 code points, and `Track.VideoBitDepth` (BitsPerChannel
  `0x55B2`). Helpers `ColorSpaceName()` / `ColorTransferName()` /
  `ColorPrimariesName()` / `ColorRangeName()` map them to the exact strings
  probers print, and `IsHDR()` reports BT.2020 + PQ/HLG signalling.
- **Frame rate** - `Track.FrameRate`, derived from `DefaultDuration` (`0x23E383`,
  `fps = 1e9 / ns`). Video tracks only (probers report `r_frame_rate` for video).
- **Codec naming** - `FFprobeCodecName()` maps mkvgo's short codec names to
  the conventional `codec_name` where they diverge (`srt`→`subrip`,
  `vobsub`→`dvd_subtitle`, `pgs`→`hdmv_pgs_subtitle`, `dvbsub`→`dvb_subtitle`).
  The existing `CodecShortName` values are intentionally kept unchanged.
- The streaming reader (`ReadStream`) parses all of the above at parity with the
  seekable reader.

### Changed

- **Behaviour change - `Language` no longer defaults to `"eng"`.** A track with
  neither a `Language` nor a `LanguageBCP47` element now reports `Language == ""`
  with `LanguagePresent == false` (previously it was synthesized to `"eng"`).
  Consumers that relied on the `"eng"` fallback must handle an empty language
  (e.g. treat empty/`und` as "undefined").
  `IsDefault` still applies the Matroska spec default (`true`) when `FlagDefault`
  is absent, but `DefaultPresent` now reports whether the flag was explicit.

### Notes / known gaps

- Colour fields reflect the **container** `Colour` element only - mkvgo does not
  decode the bitstream. When a muxer omits transfer/primaries/bit-depth from the
  container, those stay `nil`; a decoding prober may still report them from the codec VUI.
  This is an explainable difference, verified against a reference toolchain on a real
  fixture (`mkv/reader/probe_realfile_test.go`, with a live reference-prober equivalence
  test that runs when the tool is on `PATH`).
- Audio **channel layout** and **per-track bitrate** are not exposed: Matroska
  stores neither (only channel count). Left unset by design rather than fabricated.

## [0.3.1] - 2026-06-03

### Fixed

- `StreamWriter` rejects block timecodes more than int16 milliseconds (~32 s)
  from the cluster start instead of silently wrapping (`WriteBlock`,
  `WriteBlockInCurrentCluster`).

## [0.3.0] - 2026-06-02

### Added

- WebM output (`ValidateWebM`, `WriteWebM`, `NewWebMStreamWriter`, `RemuxToWebM`).
- Corruption-tolerant reader (resync to the next valid cluster).
- Bounded-memory k-way streaming for Mux/Merge/AddTrack.

### Fixed

- EBML element-ID width validation, cumulative metadata-allocation budget, and
  parser hardening (bounded recursion/allocations); streaming/seekable parser
  parity.

[0.12.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.12.0
[0.11.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.11.0
[0.10.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.10.0
[0.9.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.9.1
[0.9.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.9.0
[0.8.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.8.0
[0.7.2]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.7.2
[0.7.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.7.1
[0.7.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.7.0
[0.6.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.6.0
[0.5.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.5.0
[0.4.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.4.0
[0.3.1]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.1
[0.3.0]: https://github.com/gravity-zero/mkvgo/releases/tag/v0.3.0
