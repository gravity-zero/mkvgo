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

export interface ProbeOptions {
  /** Build the keyframe index (MKV: full scan when the file has no Cues). */
  keyframes?: boolean
  /** Read per-track BPS statistics tags (MKV; head-only via SeekHead→Tags). */
  bitrate?: boolean
  /** Parse in-band SPS for colour when container metadata is absent. */
  inbandColour?: boolean
}

export interface RemuxOptions {
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
  /** Extract one subtitle track as a WebVTT string (MKV or MP4 input). */
  extractSubtitleVTT(input: Uint8Array, trackId: number): Promise<string>
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
