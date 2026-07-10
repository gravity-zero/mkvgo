/**
 * mkvgo.ts — typed wrapper around the mkvgo WebAssembly build.
 *
 * The wasm module (built with `make wasm` → dist/wasm/mkvgo.wasm +
 * wasm_exec.js) registers a global `MkvGo` object; this wrapper loads it and
 * exposes the same API with TypeScript types. Zero dependencies, works in
 * browsers, web workers and Node ≥ 18.
 *
 * Browser:
 *   import { loadMkvGo } from './mkvgo'
 *   const mkvgo = await loadMkvGo({ wasmUrl: '/mkvgo.wasm', wasmExecUrl: '/wasm_exec.js' })
 *   const probe = await mkvgo.probe(file)          // File/Blob: head-only, any size
 *
 * Node:
 *   require(path/to/wasm_exec.js)                  // defines globalThis.Go
 *   const mkvgo = await loadMkvGo({ wasmBytes: fs.readFileSync('mkvgo.wasm') })
 */

// ---------------------------------------------------------------------------
// Result types — property names mirror the JSON the Go side emits (the same
// `json:` tags the CLI's -json output uses, so shapes are interchangeable).
// ---------------------------------------------------------------------------

export type TrackType = 'video' | 'audio' | 'subtitle'

/** One media track. Optional fields are omitted when absent, like the CLI JSON. */
export interface Track {
  id: number
  type: TrackType
  codec: string
  language?: string
  language_bcp47?: string
  name?: string
  is_default: boolean
  is_forced: boolean
  width?: number
  height?: number
  display_width?: number
  display_height?: number
  channels?: number
  sample_rate?: number
  output_sample_rate?: number
  bit_depth?: number
  video_bit_depth?: number
  codec_delay?: number
  seek_pre_roll?: number
  frame_rate?: number
  frame_count?: number
  duration_ms?: number
  bitrate?: number
  profile?: string
  level?: number
  pixel_format?: string
  field_order?: string
  rotation?: number
  color_space?: string
  color_transfer?: string
  color_primaries?: string
  color_range?: string
  hdr?: unknown
  dolby_vision?: unknown
  /** Derived display fields (same as the CLI -json output). */
  codec_long_name?: string
  channel_layout?: string
  avg_frame_rate?: number
  /** Remaining probe fields; see docs/library.md for the full Track reference. */
  [key: string]: unknown
}

export interface Chapter {
  id: number
  title: string
  start_ms: number
  end_ms: number
  sub_chapters?: Chapter[]
  [key: string]: unknown
}

export interface ProbeResult {
  /** Sniffed container: 'mkv' covers Matroska/WebM, 'mp4' covers MP4/MOV. */
  format: 'mkv' | 'mp4'
  info: { title: string; muxing_app: string; writing_app: string; [key: string]: unknown }
  duration_ms: number
  tracks: Track[]
  chapters: Chapter[]
  attachments: { id: number; name: string; mime_type: string; size: number; [key: string]: unknown }[]
  tags: unknown[]
  /** Video keyframe timestamps (ms), when requested via options.keyframes. */
  keyframes?: number[]
  /** MP4 only: tracks the probe saw but does not carry (e.g. cover art). */
  dropped_tracks?: DroppedTrack[]
  [key: string]: unknown
}

export interface DroppedTrack {
  ID: number
  Type: TrackType
  Codec: string
  Reason: string
}

export interface AbortOptions {
  /** Abort the in-flight operation (e.g. from an effect cleanup). */
  signal?: AbortSignal
}

/** One track's block-header stats from analyze(); see AnalyzeReport. */
export interface TrackStats {
  track_id: number
  type: TrackType
  codec: string
  /** Exact frame count, lacing expanded (a laced audio block counts as several frames). */
  frames: number
  /** Stored (Simple)Block/BlockGroup count; equals frames on an unlaced track. */
  packets: number
  keyframes: number
  bytes: number
  duration_ms: number
  avg_bitrate_bps: number
  peak_bitrate_bps: number
  /** Video only: frame-count spans between consecutive keyframes. */
  min_gop_frames?: number
  max_gop_frames?: number
  avg_gop_frames?: number
  keyframe_every_ms_avg?: number
  /** Video only: a decode-free heuristic hinting at B-frame reordering. */
  reordered?: boolean
  frame_rate_avg?: number
}

/**
 * Result of analyze(): a structural, no-decode stream-statistics pass over a
 * Matroska/WebM file (exact frame/keyframe counts, bitrate, GOP spans, a
 * duration reconciliation) - the mkvgo equivalent of a frame-accurate stream
 * stats probe, computed from block headers alone, never a decoded sample.
 */
export interface AnalyzeReport {
  /** The container's TRUE duration: the latest track end seen during the walk. */
  duration_ms: number
  /** The Segment Info Duration element, for comparison against duration_ms. */
  declared_duration_ms: number
  overall_bitrate_bps: number
  cluster_count: number
  block_count: number
  tracks: TrackStats[]
  /** Timing sanity issues found during the walk (duration mismatch, backward
   * timecode jump, a track with zero frames, ...). */
  warnings?: string[]
}

export type Verdict = 'direct-play' | 'remux' | 'transcode'

/** One track's playability verdict against a target; see PlayabilityReport. */
export interface TrackVerdict {
  track_id: number
  type: TrackType
  verdict: Verdict
  /** Why remux or transcode; empty for direct-play. */
  reasons?: string[] | null
}

/**
 * Result of playability(): the whole-file verdict against a playback target,
 * decided from head-only track metadata alone (codec, profile, level, pixel
 * format, resolution, colour/HDR/Dolby Vision, audio channels/sample rate) -
 * no block walk, no decode.
 */
export interface PlayabilityReport {
  target: string
  overall_verdict: Verdict
  /** The cheapest container that would make every track direct-play, when
   * overall_verdict is "remux"; empty otherwise. */
  remux_container?: string
  tracks: TrackVerdict[]
}

/** One ABR ladder rung recommended by ladder(). Guidance, not a guarantee -
 * mkvgo never transcodes; the actual encode is always an external step. */
export interface Rung {
  width: number
  height: number
  bitrate_kbps: number
  /** "2160p", "1080p", "720p", "480p", "360p". */
  label: string
}

export type ServingStrategy = 'direct-play' | 'remux-hls' | 'transcode'

/**
 * Result of ingest(): a whole-file serving decision (see ServingPlan below).
 * Field names are the snake_case JSON keys the Go struct emits.
 */
export interface ServingPlan {
  strategy: ServingStrategy
  target: string
  /** Sniffed source container: 'mkv' (also covers WebM) or 'mp4'. */
  source_container: string
  /** Cheapest container that keeps every kept track's codec; set only when
   * strategy is 'remux-hls'. */
  remux_container?: string
  /** Whether the source already carries a head-discoverable Cues index. */
  has_seek_index: boolean
  /** Set when strategy is 'remux-hls' and the source has no head-discoverable
   * seek index yet - a reindex is required before on-demand HLS can seek
   * into it. This wasm binding never performs that reindex itself (see
   * docs/wasm.md); a server/CLI runs it out of band. */
  needs_reindex: boolean
  /** Always false in wasm: reindex is never performed by this binding. */
  reindexed: boolean
  /** Whether an in-place reindex would work for this file's layout, once one
   * was attempted; meaningless here since reindex never runs in wasm. */
  reindex_in_place_possible: boolean
  playability?: PlayabilityReport
  /** Populated only when options.analyze was set. */
  analysis?: AnalyzeReport
  /** Populated only when strategy is 'transcode'. */
  ladder?: Rung[]
  /** Human-readable decision trail, one short line per decision made, in order. */
  reasons: string[]
}

export interface IngestOptions extends AbortOptions {
  /** Playback target name (see playability's target); defaults to "mse-generic". */
  target?: string
  /** Also run analyze() and attach its report (Analysis), regardless of the
   * decided strategy - forces a full block-header walk instead of head-only. */
  analyze?: boolean
}

/** One track's content-identity digest from fingerprint(); see FingerprintReport. */
export interface TrackFingerprint {
  track_id: number
  type: TrackType
  codec: string
  /** Hex SHA-256 over the track's frame payloads, in decode order. */
  sha256: string
}

/**
 * Result of fingerprint(): a container-independent content identity - the
 * same audio/video/subtitle payloads produce the same `presentation` hash
 * whether the source is Matroska or WebM, independent of track order or
 * container metadata, so a media library can detect re-muxes of the same
 * content without a byte-for-byte container comparison.
 */
export interface FingerprintReport {
  /** Stable hex SHA-256 identity for the whole file's content. */
  presentation: string
  /** One digest per track, in Container.Tracks order. */
  tracks: TrackFingerprint[]
}

export interface ProbeOptions extends AbortOptions {
  /** Build the keyframe index (MKV: full scan when the file has no Cues). */
  keyframes?: boolean
  /** Read per-track BPS statistics tags (MKV; head-only via SeekHead→Tags). */
  bitrate?: boolean
  /** Parse in-band SPS for colour when container metadata is absent. */
  inbandColour?: boolean
}

export interface RemuxOptions extends AbortOptions {
  /** Put the moov before the mdat (streaming-friendly MP4). */
  fastStart?: boolean
  /** Drop unsupported tracks instead of failing. */
  skipUnsupported?: boolean
  /** Flatten ASS/SSA subtitles to plain text instead of dropping them. */
  flattenSubs?: boolean
  /** Keep WebVTT subtitles as native wvtt samples. */
  nativeWebVTT?: boolean
  /** Signal the MP3 decoder delay as an edit list. */
  mp3ContainerDelay?: boolean
  /** Store per-track content SHA-256 for later verification. */
  contentHashes?: boolean
  /** HLS only: target segment duration in seconds (default 6). */
  segmentSeconds?: number
  /**
   * HLS/PlanHLS only: shift every subtitle cue's timing by this many
   * milliseconds (negative allowed) - a virtual per-session resync, no file
   * rewritten: re-open with a new offset and the same source serves a
   * different sync instantly. 0 (the default) leaves cues untouched.
   */
  subOffsetMs?: number
}

/**
 * Common Encryption (CENC, ISO/IEC 23001-7) packaging: sample-level AES-CTR
 * ("cenc") or AES-CBC pattern ("cbcs"), with a caller-supplied key/IV.
 * Packaging only - no license server, no DRM handshake; the caller owns key
 * delivery (keyURI, or a real license server for an EME-capable player).
 * PSSH boxes are not exposed by this wasm build (v1); build them
 * server-side and inject them if a DRM system needs one.
 */
export interface CENCOptions {
  /** "cenc" (AES-CTR) or "cbcs" (AES-CBC, 1:9 pattern on video). */
  scheme: 'cenc' | 'cbcs'
  /** The 16-byte AES key. Never written to the output. */
  key: Uint8Array
  /** The 16-byte key identifier (tenc's default_KID). */
  keyId: Uint8Array
  /** Base IV: 8 or 16 bytes for "cenc", 16 bytes (a full AES block) for "cbcs". */
  iv: Uint8Array
  /**
   * What the HLS EXT-X-KEY line advertises as URI="..." and what an
   * EME-capable player's license request ultimately needs to resolve. Left
   * empty, it defaults to a data: URI embedding key directly - convenient for
   * local testing, but it puts the raw key in the playlist text; production
   * deployments should always set a real keyURI.
   */
  keyURI?: string
}

/**
 * AES-128 whole-segment HLS encryption (RFC 8216): every media segment is
 * encrypted as one AES-CBC blob and the playlists carry an EXT-X-KEY line. The
 * counterpart of the CLI --aes-key/--aes-key-uri flags. Simpler than CENC (no
 * per-sample subsamples, no EME) and played by any HLS client that fetches the
 * key from keyURI. Packaging only - the caller owns key delivery and access
 * control around keyURI.
 */
/** One AES-128 key and its advertised delivery (single key or one rotation period). */
export interface HLSKey {
  /** The 16-byte AES-128 key. Never written to the output. */
  key: Uint8Array
  /**
   * What the EXT-X-KEY line advertises as URI="..." - typically an
   * authenticated endpoint returning the 16 key bytes.
   */
  keyURI?: string
  /**
   * A fixed 16-byte IV used for every segment in this key's periods and
   * advertised as the IV attribute. Leave unset for the spec default: each
   * segment's media sequence number is the IV and no IV attribute is written.
   */
  iv?: Uint8Array
}

export interface HLSEncryption extends HLSKey {
  /**
   * Rotate the key every N media segments through `keys`, cycling back to the
   * first once the last is used (forward secrecy: a captured key decrypts only
   * its own period, not the whole video). The media playlist then carries a
   * fresh EXT-X-KEY at each period boundary. Leave unset for a single key (the
   * `key`/`keyURI`/`iv` fields above). Needs at least two `keys`.
   */
  rotateEverySegments?: number
  /** The rotation period keys, in order. Required when rotateEverySegments is set. */
  keys?: HLSKey[]
}

/**
 * Options shared by openHLS and openABR: RemuxOptions plus one encryption
 * scheme. Set at most one of `encrypt` (AES-128 whole-segment) or `cenc`
 * (Common Encryption); setting both is rejected.
 */
export interface HLSOptions extends RemuxOptions {
  /** Package every media segment under AES-128 whole-segment encryption. */
  encrypt?: HLSEncryption
  /** Package every media segment under Common Encryption. */
  cenc?: CENCOptions
}

/** Options for openConcat: RemuxOptions plus language-based track selection. */
export interface ConcatOptions extends RemuxOptions {
  /**
   * Keep only the video track(s) plus audio/subtitle tracks whose language
   * matches, resolved from the first source's metadata - the wasm
   * counterpart of the CLI's --keep-lang. Ignored when keepTracks is set.
   */
  keepLangs?: string[]
}

export interface RemuxResult {
  data: Uint8Array
  droppedTracks: DroppedTrack[]
}

export interface HLSResult {
  /** File name → content: master.m3u8, playlist.m3u8, init.mp4, seg*.m4s, sub*. */
  files: Record<string, Uint8Array>
  droppedTracks: DroppedTrack[]
}

/** One on-demand HLS resource: its bytes, the Content-Type to serve it with,
 * and a stable content SHA-256 (hex) — mkvgo's outputs are deterministic, so
 * this is a ready-made `ETag` for HTTP caching / CDN dedup. */
export interface HLSResource {
  data: Uint8Array
  contentType: string
  sha256: string
}

/**
 * An on-demand HLS presentation over one source. Nothing is pre-generated:
 * each resource is built when requested, and a Blob/File source is read
 * through ranged slices — playing a file far larger than memory stays
 * memory-bounded.
 */
export interface HLSPlanHandle {
  /** Number of media segments (segNNNNN.m4s). */
  numSegments: number
  /** Every resource name a player requests: playlists, init, segments, subtitles. */
  resources: string[]
  /** Build one resource by its player-facing name (e.g. "seg00042.m4s"). */
  resource(name: string, options?: AbortOptions): Promise<HLSResource>
  /** Build the n-th (0-based) media segment. */
  segment(n: number, options?: AbortOptions): Promise<Uint8Array>
  /** Release the handle's callbacks. */
  close(): void
}

/**
 * An on-demand multi-variant (ABR) HLS presentation over several pre-encoded
 * quality variants of one title. The reference variant (v1) supplies the shared
 * audio and subtitles; every variant contributes its video rendition. Resource
 * names are "master.m3u8" for the top manifest and "v{k}/<name>" for a
 * variant's resource. Blob variants are read through ranged slices, so a
 * client-side ABR ladder of huge local files stays memory-bounded.
 */
export interface ABRPlanHandle {
  /** Number of quality variants. */
  numVariants: number
  /** Every resource name: "master.m3u8" and each variant's "v{k}/<name>". */
  resources: string[]
  /** Build one resource (e.g. "master.m3u8" or "v2/seg00007.m4s"). */
  resource(name: string, options?: AbortOptions): Promise<HLSResource>
  /** Release the handle's callbacks. */
  close(): void
}

/**
 * An on-demand concatenated HLS presentation over several sources played as
 * ONE continuous session (e.g. consecutive episodes), no player reload.
 * Resource names are "master.m3u8"/"playlist.m3u8"/"audio{j}.m3u8"/
 * "sub{j}.m3u8"/"sub{j}.vtt" for the top-level (concatenated) presentation,
 * and "p{k}/<name>" (k 0-based) for a resource of part k. Blob sources are
 * read through ranged slices, so a concatenated session over huge local
 * files stays memory-bounded.
 */
export interface ConcatPlanHandle {
  /** Number of parts (sources) the session carries. */
  numParts: number
  /** Every resource name: top-level playlists/subtitles plus each part's "p{k}/<name>". */
  resources: string[]
  /** Build one resource (e.g. "master.m3u8" or "p1/seg00007.m4s"). */
  resource(name: string, options?: AbortOptions): Promise<HLSResource>
  /** Release the handle's callbacks. */
  close(): void
}

/**
 * A forensic A/B session-watermarking presentation: two GOP-aligned encodes of
 * one title served as one HLS stream whose per-segment bytes are drawn from
 * variant A or B by a per-viewer bit pattern. The manifest is shared across
 * every viewer; the watermark is which variant each segment carries. The caller
 * owns the code assignment (which session gets which bits).
 */
export interface WatermarkPlanHandle {
  /** Number of media segments (shared by both variants). */
  numSegments: number
  /** The shared master playlist. */
  masterPlaylist: Uint8Array
  /** The shared media playlist (identical for every session). */
  mediaPlaylist: Uint8Array
  /** The shared init segment. */
  init: Uint8Array
  /** Build segment n from variant B when fromB is true, else variant A. */
  segment(n: number, fromB?: boolean, options?: AbortOptions): Promise<Uint8Array>
  /**
   * Build segment n routed by the session's bit code: bit n of pattern
   * (LSB-first within each byte) selects B when set, A when clear.
   */
  segmentForPattern(n: number, pattern: Uint8Array, options?: AbortOptions): Promise<Uint8Array>
  /** Release the handle's callbacks. */
  close(): void
}

export interface MkvGoApi {
  version(): string
  /**
   * Read a file's full metadata. A Uint8Array is read in place; a Blob/File is
   * read through ranged slices — head-only, so probing works on files far
   * larger than memory (a 40 GB File transfers a few hundred kilobytes).
   */
  probe(input: Uint8Array | Blob, options?: ProbeOptions): Promise<ProbeResult>
  /** Remux MKV/WebM → MP4 (no transcoding). Input must fit in memory. */
  remuxToMP4(input: Uint8Array, options?: RemuxOptions): Promise<RemuxResult>
  /** Remux MP4/MOV → MKV (no transcoding). */
  remuxFromMP4(input: Uint8Array, options?: RemuxOptions): Promise<RemuxResult>
  /** Remux MKV → WebM (VP8/VP9/AV1 + Opus/Vorbis only). */
  remuxToWebM(input: Uint8Array): Promise<RemuxResult>
  /** Package MKV/WebM as fragmented-MP4 HLS (master + media playlists + segments). */
  remuxToHLS(input: Uint8Array, options?: RemuxOptions): Promise<HLSResult>
  /**
   * Open an on-demand HLS presentation: resources are built as requested
   * instead of all at once. A Blob/File input is read through ranged slices,
   * so even a huge local file plays with bounded memory. The source must
   * carry a Cues index.
   */
  openHLS(input: Uint8Array | Blob, options?: HLSOptions): Promise<HLSPlanHandle>
  /**
   * Open an on-demand multi-variant (ABR) HLS presentation from several
   * pre-encoded quality variants of one title, best first. One handle serves
   * the whole ladder — master plus every variant's resources — built on demand.
   * Blob/File variants are read through ranged slices (memory-bounded).
   */
  openABR(inputs: (Uint8Array | Blob)[], options?: HLSOptions): Promise<ABRPlanHandle>
  /**
   * Open an on-demand concatenated HLS presentation over several sources
   * played as ONE continuous session, in playback order. One handle serves
   * the whole concatenation - top-level playlists plus every part's own
   * resources - built on demand. Blob/File sources are read through ranged
   * slices (memory-bounded).
   */
  openConcat(inputs: (Uint8Array | Blob)[], options?: ConcatOptions): Promise<ConcatPlanHandle>
  /**
   * Open a forensic A/B session-watermarking presentation from two GOP-aligned
   * encodes (a, b) of one title. The handle serves shared playlists plus
   * per-segment bytes routed to A or B by a per-viewer bit, so a leaked copy
   * carries a signature identifying the session. No re-encode; the code
   * assignment (which session gets which bits) is the caller's.
   */
  openWatermark(a: Uint8Array | Blob, b: Uint8Array | Blob, options?: HLSOptions): Promise<WatermarkPlanHandle>
  /** Extract one subtitle track as a WebVTT string (MKV or MP4 input). */
  extractSubtitleVTT(input: Uint8Array, trackId: number): Promise<string>
  /**
   * Structural, no-decode stream-statistics pass (exact frame/keyframe
   * counts, bitrate, GOP spans, duration reconciliation). Unlike probe this
   * needs a full block-header walk, so a Blob/File is read through ranged
   * slices to stay memory-bounded rather than head-only.
   */
  analyze(input: Uint8Array | Blob, options?: AbortOptions): Promise<AnalyzeReport>
  /**
   * Whole-file playability verdict against a playback target - "direct-play",
   * "remux" (with the cheapest container that would work), or "transcode" -
   * decided from head-only metadata alone. target defaults to "mse-generic";
   * see docs/wasm.md for the full list of built-in target names. An unknown
   * target rejects.
   */
  playability(input: Uint8Array | Blob, target?: string, options?: AbortOptions): Promise<PlayabilityReport>
  /**
   * Recommends an ABR ladder (resolution/bitrate rungs) from the source's
   * video track, head-only. Never upscales and never exceeds the source
   * bitrate; guidance only, mkvgo never transcodes.
   */
  ladder(input: Uint8Array | Blob, options?: AbortOptions): Promise<Rung[]>
  /**
   * One-call onboarding decision for how a source should be served to a
   * target: composes playability, ladder and a seek-index check into a
   * single ServingPlan. Read-only in wasm - the `Reindex` repair step never
   * runs here (see ServingPlan.NeedsReindex / docs/wasm.md); a server/CLI
   * performs that out of band. `options.analyze` also attaches a full
   * AnalyzeReport. An unknown `options.target` rejects.
   */
  ingest(input: Uint8Array | Blob, options?: IngestOptions): Promise<ServingPlan>
  /**
   * Container-independent content identity: a per-track SHA-256 over frame
   * payloads (decode order) plus a Presentation hash for whole-file dedup of
   * re-muxed content, regardless of track order or container metadata. This
   * is a FULL read (every frame payload is hashed), like analyze; Matroska/
   * WebM sources only.
   */
  fingerprint(input: Uint8Array | Blob, options?: AbortOptions): Promise<FingerprintReport>
}

// ---------------------------------------------------------------------------
// Loader
// ---------------------------------------------------------------------------

export interface LoadOptions {
  /** URL of mkvgo.wasm (browser; fetched with instantiateStreaming). */
  wasmUrl?: string
  /** The wasm binary itself (Node, or a custom fetch). */
  wasmBytes?: ArrayBuffer | Uint8Array
  /**
   * URL of Go's wasm_exec.js runtime, injected as a <script> when
   * globalThis.Go is not already defined (browser convenience). In Node or a
   * bundler, load wasm_exec.js yourself before calling loadMkvGo.
   */
  wasmExecUrl?: string
}

declare global {
  // Provided by Go's wasm_exec.js.
  var Go: new () => { importObject: WebAssembly.Imports; run(i: WebAssembly.Instance): Promise<void> }
  var MkvGo: MkvGoApi | undefined
}

let loaded: Promise<MkvGoApi> | null = null

/**
 * Load the mkvgo wasm module (idempotent — subsequent calls return the same
 * instance). Provide either wasmUrl (browser) or wasmBytes (Node).
 */
export function loadMkvGo(options: LoadOptions): Promise<MkvGoApi> {
  if (!loaded) loaded = doLoad(options)
  return loaded
}

async function doLoad(options: LoadOptions): Promise<MkvGoApi> {
  if (typeof globalThis.Go === 'undefined') {
    if (!options.wasmExecUrl) throw new Error('mkvgo: load wasm_exec.js first, or pass wasmExecUrl')
    await injectScript(options.wasmExecUrl)
  }
  const go = new globalThis.Go()
  let instance: WebAssembly.Instance
  if (options.wasmBytes) {
    ;({ instance } = await WebAssembly.instantiate(options.wasmBytes as BufferSource, go.importObject))
  } else if (options.wasmUrl) {
    ;({ instance } = await WebAssembly.instantiateStreaming(fetch(options.wasmUrl), go.importObject))
  } else {
    throw new Error('mkvgo: pass wasmUrl or wasmBytes')
  }
  void go.run(instance) // runs for the module's lifetime
  while (typeof globalThis.MkvGo === 'undefined') await new Promise((r) => setTimeout(r, 5))
  return globalThis.MkvGo
}

function injectScript(url: string): Promise<void> {
  return new Promise((resolve, reject) => {
    if (typeof document === 'undefined')
      return reject(new Error('mkvgo: no document — load wasm_exec.js manually in this environment'))
    const s = document.createElement('script')
    s.src = url
    s.onload = () => resolve()
    s.onerror = () => reject(new Error(`mkvgo: failed to load ${url}`))
    document.head.appendChild(s)
  })
}

// ---------------------------------------------------------------------------
// Streaming sugar
// ---------------------------------------------------------------------------

/**
 * The plan's video rendition as a progressive ReadableStream: the init
 * segment, then each media segment, built on demand as the consumer pulls —
 * pipe it to a file, a fetch Response, or an MSE feeder. Cancelling the
 * stream aborts the in-flight segment build.
 */
export function hlsSegmentStream(plan: HLSPlanHandle): ReadableStream<Uint8Array> {
  let n = -1
  const ctl = new AbortController()
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        if (n < 0) {
          controller.enqueue((await plan.resource('init.mp4', { signal: ctl.signal })).data)
        } else if (n < plan.numSegments) {
          controller.enqueue(await plan.segment(n, { signal: ctl.signal }))
        } else {
          controller.close()
          return
        }
        n++
      } catch (e) {
        controller.error(e)
      }
    },
    cancel() {
      ctl.abort()
    },
  })
}
