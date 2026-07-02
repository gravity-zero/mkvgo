# Changelog

All notable changes to mkvgo are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and the project follows
[Semantic Versioning](https://semver.org/).

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
- **Split rebases chapters.** Each segment now carries only the chapters
  overlapping its range, clipped and shifted to the segment timeline (previously
  every part carried the full source list at absolute timestamps).
- **`validate`/`compare` exit codes.** `validate` exits `1` when error-severity
  issues are found (warnings are printed but do not fail; `-strict` makes them
  fail too) and `compare` when the files differ — both previously always exited
  `0`, so neither could gate a script.
- **`Merge` no longer discards attachments.** `MuxOptions` gained
  `Title`/`Attachments`; `Merge` carries the first input's title, chapters, tags
  and attachments (first-wins, now documented). `MergeOptions.Progress` was dead —
  it is now honoured.
- **`RemoveTrack` drops orphan tags.** Tags targeting a removed track's UID are
  no longer carried into the output (they pointed at a track that no longer
  exists); global tags and tags on kept tracks survive.
- **`mp4.OpenMeta` godoc** claimed Tags stay nil; they are populated from the
  iTunes `ilst` atoms (and the title from `©nam`) since 0.11.

### Added

- **QuickTime `.mov` support (non-faststart).** The MP4 reader now parses the
  layout raw iPhone/camera QuickTime files use — `wide` + `mdat` first, `moov`
  at the end — which previously failed with `box ... has invalid size`: the
  audio SoundDescription version 1 (16 extra per-packet bytes before the
  extension boxes) and version 2 (64-byte struct, float64 sample rate) are now
  honoured, and an esds wrapped in a QuickTime `wave` extension is unwrapped.
  `OpenMeta` and `RemuxFromMP4` work on such files (real-muxer fixture added);
  output verified decodable by ffmpeg.
- **`add-attachment` / `remove-attachment`.** Attach a file (font, cover art —
  MIME sniffed from magic bytes, `-mime` to override) or remove one by ID or
  exact name; removal fails before writing anything when nothing matches.
- **Chapters as OGM text.** `set-chapters` replaces a file's chapters from the
  `CHAPTER01=…`/`CHAPTER01NAME=…` format mkvmerge and ffmpeg understand;
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
  now carries mkvmerge-style statistics tags — `BPS`, `DURATION`,
  `NUMBER_OF_FRAMES`, `NUMBER_OF_BYTES` per track, keyed by track UID —
  accumulated while streaming and written in a Tags element the SeekHead points
  to, so `matroska.WithBitrate()` (and ffprobe's `TAG:BPS`) read them head-only.
- **VP9 → MP4.** VP9 tracks remux to a `vp09` sample entry (VP9-in-ISOBMFF).
  The `vpcC` configuration record is taken from the Matroska CodecPrivate when
  one is present, and otherwise derived from the first keyframe's uncompressed
  header (profile, bit depth, chroma subsampling, colour range) plus the
  track's colour code points. `from-mp4` reads `vp09` back (the `vpcC` becomes
  the CodecPrivate, so colour survives). Output verified decodable by ffmpeg.
- **Cover art across MKV ↔ MP4.** `to-mp4` carries the source's first JPEG/PNG
  image attachment (one named `cover.*` preferred) as the iTunes `covr` atom;
  `from-mp4`/`OpenMeta` bring a `covr` back as a Matroska attachment
  (`cover.jpg`/`cover.png`). Fonts and other non-image attachments are still
  not representable in MP4.
- **`split -range` accepts clock times.** Each range bound can be milliseconds
  (`300000`), fractional seconds (`90.5`) or `[HH:]MM:SS[.fraction]` (`5:00`,
  `01:30:00`).
- **Seekable WebM output.** `RemuxToWebM`/`to-webm` now writes a real indexed
  file — known-size clusters, a Cues seek index and a SeekHead — instead of the
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
  encoder padding at the tail — their delay is intrinsic to the bitstream and ffmpeg
  does not trim that tail from an MP4 either. Two changes make this hold:
  - New `Track.CodecDelay` / `Track.SeekPreRoll` (Matroska `0x56AA`/`0x56BB`, ns) are
    read and written. A codec's container-signalled encoder/decoder delay (the MP4
    edit-list `media_time`) is carried as `CodecDelay` on the MKV side and re-emitted
    as an MP4 edit list (`elst`) on the way back, for AAC/AC-3/E-AC-3. Opus, MP3 and
    Vorbis carry their delay intrinsically (Opus pre-skip in the OpusHead, MP3 in its
    in-band Xing/LAME header), which ffmpeg's decoder applies regardless of the
    container, so they are excluded — a derived edit list would over-trim them;
    FLAC/DTS/PCM have no priming. (`Options.MP3ContainerDelay` /
    `--mp3-container-delay` opts MP3 back into the edit-list path, to round-trip an
    MP3 that originated in an MP4 — rare; it over-trims a native-MKV MP3, so it is off
    by default.)
  - **Audio tracks are written on a sample-rate media timescale** (`mdhd`), as ffmpeg
    does, instead of the millisecond movie timescale. This makes the edit list's
    `media_time` sample-exact, which is required for ffmpeg to trim a codec's priming
    precisely — notably AC-3, whose decoder delay it ignores from a millisecond-
    quantised edit list. Video/text/subtitle timing is unchanged.
- **Per-track bitrate from Matroska tags.** The `BPS` tag ffmpeg/mkvmerge write per
  track (bits per second) is now surfaced as the typed `Track.Bitrate`, keyed by the
  track UID. This is the value ffprobe exposes as `TAG:BPS` — ffprobe leaves its own
  per-stream `bit_rate` field `N/A` for Matroska, so this gives more than ffprobe
  there; for MP4 `Track.Bitrate` comes from `btrt`/`esds` and equals ffprobe's
  `bit_rate`. A full `Read` fills it; the metadata-only probe does too under the new
  `matroska.WithBitrate()` / `reader.WithBitrate()` option, which follows the head
  `SeekHead` straight to the `Tags` element (one seek, no Cluster scan — the muxer
  references `Tags` from the head). Off by default, so the default probe stays minimal.
- **Extended disposition flags.** `Track.HearingImpaired`, `VisualImpaired`,
  `TextDescriptions`, `Original` and `Commentary` expose the Matroska
  FlagHearingImpaired/…/FlagCommentary elements — the ffprobe stream dispositions
  of the same name — alongside the existing default/forced. Shown in `probe` and
  JSON. Matroska-only (MP4 has no equivalent boxes).
- **3D stereo + 360 projection.** `Track.StereoMode` (with `StereoModeName()`) and
  `Track.Projection` report stereoscopic-3D arrangement and spherical/360
  projection — from the Matroska StereoMode/Projection elements or the MP4
  `st3d`/`sv3d` boxes (st3d mapped to the Matroska StereoMode values). Shown in
  `probe` and JSON; unset for ordinary 2D video.
- **Average frame rate.** `Track.AvgFrameRate()` returns ffprobe's
  `avg_frame_rate` (frame count over duration) — non-zero for MP4 video
  (head-only), 0 for Matroska where the header carries no frame count. Surfaced in
  `probe` (when it diverges from the nominal rate, i.e. VFR) and JSON.
- **HDR10 static metadata.** `Track.HDR` (`HDRStaticMetadata`) now carries the
  Content Light Level (`MaxCLL`/`MaxFALL`, cd/m²) and the SMPTE ST 2086 Mastering
  Display colour volume (`MasteringDisplay`: R/G/B + white-point CIE 1931
  chromaticities and the luminance range), read head-only from the Matroska
  Colour element (`MaxCLL`/`MaxFALL` + `MasteringMetadata`) or the MP4 `clli`/
  `mdcv` boxes — the frame side data ffprobe reports, the last colour/HDR gap
  versus a head-only probe (HDR detection, CICP colour and Dolby Vision were
  already covered). mdcv's fixed-point values (and its G,B,R primary order) are
  normalised to the same units as the Matroska floats. Shown in `probe` output
  and JSON (`hdr`). nil when the stream carries no such metadata.

### Fixed

- **A/V presentation offset preserved across MP4.** A per-track start offset (the
  empty edit ffmpeg writes for an `-itsoffset`/sync correction — e.g. audio starting
  476 ms after video) was read into the block timestamps but never re-emitted by
  `to-mp4`: each track was rebased to 0, silently desyncing the audio. `to-mp4` now
  writes the offset as a leading empty edit (`media_time -1`), so the A/V gap
  survives the round trip. Coded data was always intact; only the timing was lost.
- **Split no longer trims a frame of real audio at the seam.** Every output segment
  inherited the source's `CodecDelay` (encoder priming), but only the first segment
  actually starts with that priming — a later segment begins on real audio. A
  decoder or `to-mp4` then trimmed `CodecDelay` worth of real samples (one AAC frame,
  ~23 ms) at each cut. Later segments now drop `CodecDelay`, so a split is lossless:
  no coded packet is dropped and the decoded sample count is exact. (Decoding a later
  segment in isolation still differs from the source within the segment — AAC frames
  share overlap-add context across the cut, so a clean boundary is impossible without
  re-encoding; this is codec physics, not a loss. The coded stream is intact.)
- **Last chapter's end time preserved across MP4.** The Nero `chpl` box carries only
  start times, so `from-mp4` closed each chapter at the next one's start and left the
  last open (read back as `0`). The last chapter now closes at the movie end, so an
  explicit final `ChapterTimeEnd` survives the round trip.
- **Container title, global tags and track name now map to MP4.** `to-mp4` carried a
  track's language but dropped the container title, the other global tags and the
  per-track name. All are now written where the tooling reads them: the title as
  `moov/udta/meta/ilst/©nam` (ffprobe's format `title`); the other global tags as
  their iTunes atoms (`ARTIST`→`©ART`, `ALBUM`→`©alb`, `DATE_RELEASED`→`©day`,
  `GENRE`→`©gen`, `COMMENT`→`©cmt`, `COMPOSER`→`©wrt`, `DESCRIPTION`→`desc`,
  `ENCODER`→`©too`); and the track name as the `hdlr` name (ffprobe's stream
  `handler_name`, the only per-track string MP4 exposes — MP4 has no readable stream
  `title`) plus the QuickTime `trak/udta/name` box ffmpeg writes. `from-mp4` reads
  them all back into `Info.Title` / `Tags` / `Track.Name`, so they survive a round
  trip (and an MP4-in/MP4-out edit) instead of being lost at the MP4 boundary.
- **MP4 sample-table complexity DoS.** A constant-size `stsz` (and the keyframe
  index's frame count) was bounded only by the 134M `maxSamples` cap, so a tiny
  box could declare a huge sample count and force a multi-second, multi-GB
  allocation on a small file. The count is now bounded by the file size (samples
  must physically fit) before any allocation, on both the full-table and
  keyframe-only paths.
- **MP4 chunk/stsc quadratic.** Building the full sample table looked up the
  samples-per-chunk with a linear scan of the stsc entries *per chunk* — O(chunks ×
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
  count — relevant for an S3/HTTP `FS`.
- **Fixed-lacing residual bytes.** A fixed-lacing block whose data size is not a
  multiple of its frame count now errors instead of silently dropping the
  remainder.
- **Output Close errors surfaced.** `RemoveTrack`, `AddTrack`, `EditMetadata`,
  `Join`, `Split`, `Reindex`, `MergeSubtitle`/`MergeASS`, `ExtractSubtitle`/
  `ExtractASS` and `RemuxToWebM` now return a Close error on the success path
  (e.g. a custom `FS` that commits the write on Close), matching `Mux`.
- **Duplicate Info/Tracks (non-conformant files).** A file with more than one
  Info or Tracks element (the spec allows one each) is now handled "first wins"
  by both `Read` and `ReadMeta` — previously the full `Read` appended a second
  Tracks set, doubling the tracks, and the two readers could disagree.
- **Debuggable parse errors.** A parse failure now carries the failing element's
  ID and byte offset (`element 0x… at offset N: …`) instead of a bare error, so a
  failure on a real-world file points at where it went wrong.

### Hardened

- **CLI rejects unknown flags.** A mistyped or unsupported `-`/`--` option (e.g.
  `to-mp4 --fastart`) was silently treated as a positional argument — a confusing
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
  fails if a single parse exceeds a deadline — Go fuzzing flags panics but not
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
  colour was actually read from a source — the container Colour element, an MP4
  colr box, or the codec bitstream's colour signalling (H.264/HEVC VUI, AV1
  color_config, VP9 vpcC) — even when it resolves to "unspecified" (every Color*
  left nil). It lets a caller tell a confirmed-SDR/unspecified stream (true, no
  colour values) from one whose colour could not be read at all (false), rather
  than conflating both as a bare nil. Shown in `probe` output.
- **Complete keyframe index for Cues-less Matroska.** `WithKeyframeIndex()`
  (re-exported as `matroska.WithKeyframeIndex`) builds the *complete* video
  keyframe index — every keyframe, equal to `ffprobe -skip_frame nokey`, not a
  sample — for a Matroska that carries no Cues. After the head parse, and only
  when no Cues were found, it makes a single sequential read-ahead pass over the
  Segment (cluster by cluster, never a per-block seek), reading element headers
  and discarding block payloads in-stream — no demux, no decode. It transfers the
  Segment like ffprobe but pure-Go in-process; discarding rather than seeking past
  payloads keeps the reads sequential, so it stays I/O-bound (not seek-bound) on a
  high-latency SMB/NAS mount. Per
  Cluster it reads the Timestamp, then each SimpleBlock/BlockGroup, emitting a
  keyframe (SimpleBlock keyframe flag, or a BlockGroup with no ReferenceBlock) on
  the video track only at `Timestamp + relative-timecode`. It is the "no external
  fallback" path; `WithSampledKeyframes` remains the cheaper coarse variant. Files
  with Cues are never scanned. The CLI `keyframes` command uses it for such files.
  (Matroska has no edit list, so no time shift is applied — parity with the
  Cues-derived index.)
- **Sampled keyframe index for Cues-less Matroska.** `WithSampledKeyframes(n)`
  (re-exported as `matroska.WithSampledKeyframes`) recovers a coarse keyframe
  index for a Matroska that carries no Cues: after the head parse, and only when
  no Cues were found, it probes n evenly-spaced byte offsets in the Segment body,
  resyncing to the next Cluster at each. Within the Cluster it reads block headers
  (payloads skipped by element size) to find the first real video keyframe — a
  SimpleBlock keyframe flag, or a BlockGroup with no ReferenceBlock — and emits
  that block's exact presentation time, so every point is a genuine seek point
  even when the muxer does not align Clusters to keyframes (the Cluster start is
  NOT assumed to be a keyframe). A Cluster with no video keyframe is skipped
  (bounded). Bounded to about n seeks, no block-by-block decode, so a Cues-less
  file reports keyframes instead of forcing an external fallback. The CLI
  `keyframes` command uses it automatically for such files.
- **Fragmented-MP4 metadata.** For a fragmented MP4 (an mvex box in the moov),
  the probe now recovers the frame rate from the fragment defaults (mvex>trex)
  and the keyframe index from a random-access index — the mfra/tfra at the file
  tail, or the sidx Segment Index at the head that streaming fMP4 carries — both
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
  coalesced, so it is both I/O- and round-trip-light — about 90% fewer bytes read
  and ~2× faster on a high-latency (9p/SMB/NAS) mount, with lower variance. The
  full table is still read for remux/extract; any unexpected layout falls back to
  the full read, and the result is byte-identical.

### Fixed

- **Truncated Matroska tail.** A file cut mid-element after the head metadata
  (Info + Tracks) now returns what was parsed instead of failing with an
  unexpected EOF, as ffprobe does on a truncated file.
- **Desynced MP4 box walk.** When the top-level box walk runs into the mdat (a
  file with a wrong mdat size), `findMoov` falls back to a bounded, validated
  backward scan for the moov, recovering files ffprobe still reads.
- **Typed non-Matroska error.** `Open`/`Read`/`OpenMeta`/`ReadMeta` return the new
  `ErrNotMatroska` (matchable with `errors.Is`) when a misnamed `.mkv` is actually
  an MP4-family file, so a caller dispatching by extension can re-route to the mp4
  reader instead of getting a cryptic EBML error.

## [0.9.1] - 2026-06-25

### Added

- **In-band colour fallback (opt-in).** `reader.WithInBandColourFallback()`
  (re-exported as `matroska.WithInBandColourFallback()`) and
  `mp4.Options{InBandColour: true}` recover a video track's colour when it is
  carried only in an in-band HEVC SPS — a bare hvcC with no parameter sets and no
  container colour, as some streaming-style HDR muxes write. The head parse is
  unchanged; only a track that still lacks colour reads a bounded slice of its
  first sample, parses the SPS VUI, and applies an Alternative Transfer
  Characteristics SEI (payload type 147) override — HLG's `bt2020-10` →
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

Field-equivalence pass: the metadata probe reports more of what ffprobe does,
validated against ffprobe over a sweep of real files.

### Added

- **Audio output sample rate for SBR (HE-AAC).** `Track.OutputSampleRate` carries
  the decoder's doubled rate (Matroska `OutputSamplingFrequency` 0x78B5, or the
  AAC `AudioSpecificConfig` SBR extension rate — explicit AOT 5 and the
  backward-compatible 0x2b7 sync extension), and `Track.EffectiveSampleRate()`
  returns what ffprobe reports as `sample_rate`. Read on both the MP4 and Matroska
  paths; the writer persists 0x78B5 for round-trip.
- **Codec level.** `Track.Level` exposes the SPS `level_idc` ffprobe reports as
  `level` (H.264, HEVC, AV1), via the shared codec-bitstream fallback.
- **Display aspect ratio for anamorphic video.** `Track.DisplayWidth` /
  `DisplayHeight` (their ratio is the display aspect), with `DisplayAspectRatio()`
  / `SampleAspectRatio()` helpers returning ffprobe's `display_aspect_ratio` /
  `sample_aspect_ratio`. Read, in precedence order, from the MP4 `pasp` box, the
  Matroska `DisplayWidth`/`DisplayHeight` (0x54B0/0x54BA) elements, or the H.264/
  HEVC SPS VUI `aspect_ratio_info` (Table E-1 `aspect_ratio_idc`, or Extended_SAR
  `sar_width:sar_height`) — the most common H.264 SAR carrier, read head-only from
  the avcC. The ratio is stored exactly (no rounding that would collapse a fine
  pixel aspect), and the helpers reduce it with the same bounded `av_reduce`
  (1024×1024) ffmpeg uses, so the `sar`/`dar` strings match ffprobe on every
  ordinary ratio and tame absurd ones. Both reader paths and the writer handle the
  elements; the MP4 remux emits a `pasp` box so anamorphic display survives a
  round trip.
- **Per-stream average bitrate.** `Track.Bitrate` from the MP4 `btrt` box (or the
  esds `avgBitrate` for AAC).
- **Pixel format.** `Track.PixelFormat` (ffprobe `pix_fmt`, e.g. `yuv420p`,
  `yuv420p10le`) composed from the codec's chroma subsampling and bit depth
  (H.264/HEVC SPS, AV1 colour config, VP9 `vpcC`). For HEVC `hev1` with in-band
  parameter sets, the 4:2:0 chroma of Main/Main 10 is taken from the `hvcC` profile,
  so it still reads head-only.
- **Field order.** `Track.FieldOrder` ("progressive"/"interlaced", ffprobe
  `field_order`) from the Matroska `FlagInterlaced` (0x9A) element or the H.264
  `frame_mbs_only_flag`.
- **Frame count.** `Track.FrameCount` (ffprobe `nb_frames`) from the MP4 `stsz`/
  `stz2` sample count — read head-only, no sample-table expansion. Matroska has no
  head-only frame count, so it stays 0 there.
- **Per-track duration.** `Track.DurationMs` (ffprobe per-stream `duration`) from
  the MP4 `mdhd` (duration ÷ media timescale), so a track that differs from the
  movie length is reported individually. Matroska carries no per-track header
  duration, so it stays 0 there.
- **MP4 file-level tags.** The metadata probe now reads the iTunes/QuickTime
  `udta`/`meta`/`ilst` atoms — `Container.Tags` gets the text tags (`©nam`→`TITLE`,
  `©ART`→`ARTIST`, `©day`→`DATE_RELEASED`, `©too`→`ENCODER`, …) and `Info.Title` is
  filled from `©nam`, matching how the Matroska reader exposes tags. Non-text atoms
  (cover art) are skipped.
- **Codec long name and channel layout.** `Track.CodecLongName()` returns ffprobe's
  `codec_long_name` (e.g. "H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10"), and
  `Track.ChannelLayout()` returns `channel_layout` ("stereo", "5.1(side)", …) from
  the channel count — display strings the fast path previously omitted.
- **MP4 frame rate is read head-only.** `Track.FrameRate` is now derived from the
  `stts` header (media timescale ÷ first `sample_delta`) — ffprobe's `r_frame_rate`
  for constant-frame-rate video — so the metadata probe reports it without
  expanding the sample table (it previously needed `Options{Keyframes: true}`).
- **Display rotation.** `Track.Rotation` (0/90/180/270, clockwise) read from the
  MP4 `tkhd` display matrix — the same matrix ffprobe exposes as Display Matrix
  side data. Lets a player show portrait phone video the right way up.
- **CLI** — `probe` prints all of the above per track (codec long name, profile/
  level, pixel format, aspect ratio, rotation, frame rate, frame count, per-track
  duration, bitrate, field order, channel layout, output sample rate); `probe -json`
  and `tracks -json` carry the same fields, including the derived `codec_long_name`
  and `channel_layout`. See [docs/library.md](docs/library.md) for the full table.

### Fixed

- **HE-AACv2 Parametric Stereo channel count.** A PS stream codes a mono core
  (`channelConfiguration` 1) the decoder upmixes to stereo; `mp4.OpenMeta` now
  reports 2 channels like ffprobe, detecting both explicit (AOT 29) and
  backward-compatible (0x2b7 / 0x548) signalling.
- **QuickTime `nclc` colr type.** The MP4 probe read only the `nclx` colr type and
  silently skipped the QuickTime `nclc` form, leaving `color_space` empty on files
  that carry their matrix only there. Both types are now read, each CICP field
  taken independently (a matrix-only stream still reports its `color_space`).

### Notes

- Documented three accepted head-only limitations — data present only in the media
  frames, not in any header the probe parses, so ffprobe surfaces it by decoding a
  frame while mkvgo reports the header values: implicit in-band SBR and Parametric
  Stereo (audio), and colour signalled only in an in-band SPS (video).
- On pathological **near-square** sample aspect ratios, ffprobe's `sample_aspect_ratio`
  can differ from mkvgo's: ffprobe re-derives the SAR from the dimension-reduced DAR
  (so the same VUI SAR prints differently at different resolutions), whereas mkvgo
  reports the exact VUI/`pasp` ratio. Display-only and imperceptible (all ≈ the same
  picture shape); mkvgo keeps the true signal rather than mirror that quirk.

### Tests

- **Greatly expanded the test surface**, driven by statement coverage and gremlins
  mutation testing. Statement coverage is now **≥ 90% in every package**
  (`cmd/mkvgo/commands` 16.6% → 97.9%, `mkv/reader` → 90.6%, `mkv/ops` → 92.0%,
  `mp4` → 92.9%; `ebml`/`mkv`/`mkv/subtitle`/`mkv/writer` 90–98%), and global
  mutation efficacy rose from 70% to 82% (mutator coverage 85% → 95%). The new
  tests cover the previously-untested error/edge paths: malformed and truncated
  inputs, bit-reader / Exp-Golomb boundaries, sample-table and EBML element/VINT
  parsing, the `av_reduce` aspect algorithm (brute-force property test), and the
  codec-bitstream skip paths (scaling lists, RPS).
- **CLI is now integration-tested.** Every command is exercised on a fixture
  (inspection via captured stdout and `-json`, edit/extract by re-reading the
  output, assembly/split/remux/reindex by parsing the produced file). To make the
  `Fatal`/error paths reachable in-process, the CLI's process exit is now an
  injectable hook (`Fatal` calls a `var osExit = os.Exit`) — behaviour is identical
  in production.

## [0.8.0] - 2026-06-23

### Added

- **MP4 metadata probe.** `mp4.OpenMeta` / `OpenMetaWithFS` / `ReadMeta` read an
  MP4's stream metadata head-only, reporting per track: **language** (`mdhd`
  ISO 639-2 — including QuickTime Macintosh language codes — and the `elng` BCP-47
  box), the **default** flag (`tkhd` track_enabled), the **forced** flag (DASH-role
  `kind` box), the **audio channel count** (from the AAC `AudioSpecificConfig` and
  AC-3/E-AC-3 `dac3`/`dec3`, not the unreliable `AudioSampleEntry` field), and
  **colour** code points (`colr`, falling back to the codec bitstream via the now
  exported `reader.FillColourFromCodecPrivate`).
- **Dropped (non-carried) tracks** are surfaced: the probe returns an additional
  `[]DroppedTrack` (cover art / attached pictures, hint/timecode/metadata tracks),
  each with its track ID, fourcc and reason. `RemuxFromMP4` reports them through
  `Options.OnDrop`. (A QuickTime chapter track is not reported — see Fixed.)
- **Dolby Vision.** `Track.DolbyVision` exposes the decoded
  `DOVIDecoderConfigurationRecord` (profile, level, RPU/EL/BL, `bl_signal_compatibility_id`),
  read from the MP4 `dvcC`/`dvvC` box (and the `dvhe`/`dvh1`/`dvav`/`dva1`/`dav1`
  sample entry types) or the Matroska `dvcC`/`dvvC` `BlockAdditionMapping`. It is
  carried through both remux directions, choosing the Dolby sample entry type
  (`dvh1`/`dva1`/`dav1`) for a non-cross-compatible stream and the plain
  `hvc1`/`avc1`/`av01` tag for a cross-compatible one. New `mkv.DolbyVision`,
  `ParseDolbyVisionConfig`, `EncodeDolbyVisionConfig`, `DolbyVision.BoxType`.
- **Head-only keyframe index** on `Container.Keyframes` ([]int64 ms, ascending,
  de-duplicated) — the cut points for `-c copy` HLS/DASH segmentation. MKV/WebM
  fills it from the `Cues` index via the `SeekHead` (one seek, no `Cluster` scan)
  in the metadata pass; MP4 fills it from the `stss`/`stts`/`ctts` tables, with the
  edit list (`elst`) applied as ffmpeg does, opt-in via `Options{Keyframes: true}`.
- **Subtitle extraction to WebVTT**, replacing an `ffmpeg -f webvtt` fork:
  `ops.ExtractSubtitleWebVTT` (embedded MKV/WebM track), `mp4.ExtractSubtitleWebVTT`
  (embedded MP4 tx3g/wvtt track) and `subtitle.FileToWebVTT` (external
  `.srt`/`.ass`/`.ssa`/`.vtt` sidecar), streaming to any `io.Writer`. Building
  blocks: `subtitle.Cue`, `WriteWebVTT`, `FormatVTTTime`, `SRTToCues`, `ASSToCues`,
  `FlattenASSBlock`, `ResolveCueEnds`.
- **Remux preserves what the probe reads.** `RemuxToMP4` writes the default flag
  (`tkhd` track_enabled), the forced flag (`kind` box) and the BCP-47 language
  (`elng`); the MKV writer persists `LanguageBCP47`. The edit list is folded into
  the composition times so `RemuxFromMP4` and the keyframe index present ffmpeg's
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
  `tref/chap`) as a dropped track — its content is already read from `chpl`.

## [0.7.2] - 2026-06-22

### Added

- **WebVTT subtitles in MP4 remux.** `RemuxToMP4` now carries WebVTT tracks
  (`S_TEXT/WEBVTT` and the WebM-era `D_WEBVTT/*` ids that ffmpeg writes) instead
  of dropping them:
  - By default a WebVTT track is carried as `tx3g` timed text — the only MP4
    subtitle form read universally (ffmpeg included).
  - **`Options.NativeWebVTT`** (CLI `--webvtt-native`) carries it losslessly as
    native `wvtt` (ISO/IEC 14496-30) instead — cue settings and inline markup are
    preserved and Apple/Safari/CMAF read it, but ffmpeg's MP4 demuxer does not, so
    it is opt-in. `RemuxFromMP4` reads `wvtt` back to `S_TEXT/WEBVTT`.
- **`Options.FlattenStyledSubs`** (CLI `--flatten-subs`) carries ASS/SSA — which
  have no native MP4 form — as `tx3g`, stripping the dialogue framing and override
  tags (`{\...}`, `\N`, `\h`). Lossy: all styling/positioning/karaoke is discarded.
- Subtitles **never fail a remux**: a subtitle whose format cannot be carried is
  now always dropped with a reason via `Options.OnDrop` (pointing at the relevant
  flag), instead of being silently skipped or — for some inputs — aborting.
- New codec short name **`webvtt`** for `S_TEXT/WEBVTT` (reader and writer), for
  parity with `srt`/`ass`/`ssa`.

## [0.7.1] - 2026-06-21

### Added

- **`mp4.OpenMeta`** / **`OpenMetaWithFS`** / **`ReadMeta`** — metadata-only probe
  of an MP4 file, the counterpart of the MKV reader's `OpenMeta`/`ReadMeta`. They
  parse only the movie header (`moov`: track sample entries, colour code points,
  chapters) and return a `*mkv.Container` (Info, Tracks, Chapters, DurationMs)
  **without reading any sample data (`mdat`) or writing an output file**. This is
  the fast path for indexing/scanning an MP4 library — previously the only way to
  read an MP4's codecs/colour/chapters was a full `RemuxFromMP4` to disk.
  `RemuxFromMP4` and the probe now build their Matroska metadata from a single
  shared helper, so they report identical tracks/chapters/duration.

## [0.7.0] - 2026-06-21

New `mp4` package: remux between Matroska/WebM and MP4 (ISO base media file
format) without transcoding. It is isolated from the EBML core (shares no
low-level code with `ebml`/`mkv`) and is experimental.

### Added

- **`mp4.RemuxToMP4`** — MKV/WebM → progressive MP4. Compressed samples are
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
- **`mp4.RemuxFromMP4`** — MP4 → MKV. Reads the codecs above plus their MP4
  sample entries; colour and chapters round-trip back to the Matroska `Colour`
  element and chapter atoms.
- **`writer.WriteBlockGroup`** — writes a BlockGroup with a BlockDuration; used
  for subtitle cues. `WriteCluster` now emits a BlockGroup for any block with a
  non-zero `Duration`.
- **`Block.Duration`** — new additive field, populated from a BlockGroup's
  BlockDuration by the block reader.
- The MKV writer now emits the `Colour` element for tracks carrying colour code
  points.
- **CLI** — `mkvgo to-mp4 [--faststart] [--skip-unsupported] <in> <out.mp4>`,
  `mkvgo from-mp4 <in.mp4> <out.mkv>`, and `mkvgo to-webm <in> <out.webm>`
  (the latter exposing the existing `RemuxToWebM` on the command line).

## [0.6.0] - 2026-06-03

`ReadMeta`/`Read` now derive colour/HDR metadata from the codec bitstream when the
container Colour element (0x55B0) is absent. Many files signal colour only in the
codec SPS/VUI; such a track previously read as having no colour. Additive — no API
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
  - **VP9** (vpcC fixed fields, when a CodecPrivate is present) — best-effort.

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
  — the colour fields stay nil and the read continues. The parsers are panic- and
  hang-free on their own — a bounds-checked Exp-Golomb reader with a capped
  leading-zero run, every bitstream-driven loop count and bit width bounded, and
  emulation-prevention stripping; the `recover()` in the dispatcher is only a
  last-resort backstop. **`FuzzCodecColour`** drives random bytes straight at the
  parsers *without* that backstop, so a missing bound surfaces instead of being
  masked — it found and fixed an out-of-range bit depth and an Exp-Golomb-driven
  loop, both kept as regression seeds.

### Codecs covered

H.264, HEVC and AV1 are covered with hermetic byte-fixture tests. VP9 (vpcC) is
best-effort: VP9 colour usually lives in the container or in per-frame headers,
the latter outside the metadata path. VVC / Dolby Vision are out of scope.

## [0.5.0] - 2026-06-03

Fast metadata-only read path for library indexing. Additive — `Read` / `Open`
are unchanged.

### Added

- **`ReadMeta(ctx, r, path)`** plus **`OpenMeta`** / **`OpenMetaWithFS`**,
  mirroring the `Read` / `Open` / `OpenWithFS` trio (also re-exported from the
  `matroska` facade). They return the same `Tracks` + `Info` (and `DurationMs`)
  as a full `Read` — byte-identical, via the same `parseInfo` / `parseTracks`
  logic — but stop as soon as both are parsed:
  - never parse the Cues index, never traverse Clusters;
  - reads are buffered (~2 KiB) so the byte-at-a-time EBML reads cost one syscall
    instead of hundreds (matters on a network-mounted library);
  - a head `SeekHead` is used to jump straight to Info/Tracks, so a file whose
    `Tracks` element sits after the first Cluster still works without scanning.
  - `Chapters`, `Attachments`, `Tags` and `Cues` are left **nil** — call
    `Read` / `Open` for those.
  - Hardened for untrusted input: a forged `SeekHead` cannot make the fast path
    over-read (the `SeekID` size is bounded to a real element-ID width and
    `SeekPosition` offsets are range-checked).

### Performance (measured)

On 5 real 5–9 GB mkvmerge files (`bench/main.go`), per file:

| read                | bytes read | time        |
|---------------------|-----------:|------------:|
| `reader.Read` (full)|   ~180 KB  | ~17,000 ms  |
| `ReadMeta`          |    ~2 KB   |    ~0.2 ms  |
| `ffprobe` (ref)     |   ~1.2 MB  |     ~50 ms  |

`ReadMeta` reads ~90× fewer bytes and is ~80,000× faster than the full `Read`,
and ~600× fewer bytes / ~250× faster than forking `ffprobe`. The full `Read`'s
cost is the Cues index (~790 KB across the five files) plus walking every
Cluster — neither needed for indexing. A media server can now use the in-process
reader for indexing instead of forking `ffprobe` per file.

## [0.4.0] - 2026-06-03

Probe metadata: the track reader now exposes the fields a media indexer needs to
match `ffprobe -show_streams`, and can distinguish "explicitly set in the file"
from "spec default". All struct changes are additive — existing exported fields
and types are unchanged.

### Added

- **Language**
  - `Track.LanguageBCP47` — the IETF BCP-47 language element (`0x22B59D`), now
    parsed alongside the legacy ISO 639-2 `Language` (`0x22B59C`). Modern muxers
    that write only BCP-47 are no longer mis-read.
  - `Track.ResolvedLanguage()` — effective language with BCP-47 taking precedence
    over the legacy element, per the Matroska spec.
- **Presence flags** — tell an explicit value from an applied default:
  `Track.LanguagePresent`, `Track.DefaultPresent`, `Track.ForcedPresent`.
- **Video colour** — parsed from the `Colour` element (`0x55B0`):
  `Track.ColorSpace` (MatrixCoefficients `0x55B1`), `Track.ColorTransfer`
  (`0x55BA`), `Track.ColorPrimaries` (`0x55BB`), `Track.ColorRange` (`0x55B9`) as
  raw CICP / ITU-T H.273 code points, and `Track.VideoBitDepth` (BitsPerChannel
  `0x55B2`). Helpers `ColorSpaceName()` / `ColorTransferName()` /
  `ColorPrimariesName()` / `ColorRangeName()` map them to the exact strings
  ffprobe prints, and `IsHDR()` reports BT.2020 + PQ/HLG signalling.
- **Frame rate** — `Track.FrameRate`, derived from `DefaultDuration` (`0x23E383`,
  `fps = 1e9 / ns`). Video tracks only (ffprobe reports `r_frame_rate` for video).
- **Codec naming** — `FFprobeCodecName()` maps mkvgo's short codec names to
  ffprobe's `codec_name` where they diverge (`srt`→`subrip`,
  `vobsub`→`dvd_subtitle`, `pgs`→`hdmv_pgs_subtitle`, `dvbsub`→`dvb_subtitle`).
  The existing `CodecShortName` values are intentionally kept unchanged.
- The streaming reader (`ReadStream`) parses all of the above at parity with the
  seekable reader.

### Changed

- **Behaviour change — `Language` no longer defaults to `"eng"`.** A track with
  neither a `Language` nor a `LanguageBCP47` element now reports `Language == ""`
  with `LanguagePresent == false` (previously it was synthesized to `"eng"`).
  Consumers that relied on the `"eng"` fallback must handle an empty language
  (e.g. treat empty/`und` as "undefined").
  `IsDefault` still applies the Matroska spec default (`true`) when `FlagDefault`
  is absent, but `DefaultPresent` now reports whether the flag was explicit.

### Notes / known gaps

- Colour fields reflect the **container** `Colour` element only — mkvgo does not
  decode the bitstream. When a muxer omits transfer/primaries/bit-depth from the
  container, those stay `nil`; ffprobe may still report them from the codec VUI.
  This is an explainable difference, verified against ffmpeg 7.1 on a real
  fixture (`mkv/reader/probe_realfile_test.go`, with a live ffprobe equivalence
  test that runs when ffprobe is on `PATH`).
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
