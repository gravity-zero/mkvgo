# mkvgo in the browser (WebAssembly)

mkvgo compiles to WebAssembly - the whole probe/remux/HLS engine runs
client-side, in browsers, web workers and Node. Zero dependencies carries over:
the artifact is **~6 MB raw, ~1.6 MB gzipped**, and nothing ever leaves the
user's machine.

What it enables:

- **Probe any file instantly, whatever its size.** A `File`/`Blob` is read
  through ranged slices, and mkvgo's probe is head-only - inspecting a 40 GB
  MKV in a `<input type=file>` transfers a few hundred kilobytes and takes
  milliseconds. No upload, no server.
- **Remux MKV → MP4 (and back), package HLS, extract subtitles** - for files
  that fit in memory - without transcoding: the original frames are copied
  into the new container.
- **Play an MKV in a `<video>` tag** with no server and no transcoding:
  `openHLS(file)` builds fragmented-MP4 segments **on demand from ranged
  reads of the File** - a huge local file plays through Media Source
  Extensions with bounded memory (see the [runnable
  demo](../web/example/index.html)); `remuxToHLS` is the eager, all-at-once
  variant for files that fit in memory.

## Build

```bash
make wasm          # → dist/wasm/mkvgo.wasm + wasm_exec.js (Go's JS runtime)
make wasm-smoke    # build + run the Node end-to-end smoke test
```

Serve both files; `wasm_exec.js` must be loaded before instantiating the
module. Serving `mkvgo.wasm` with `Content-Encoding: gzip`/`br` and
`Content-Type: application/wasm` (for `instantiateStreaming`) is recommended.

## API

The module registers a global `MkvGo`; **[`web/mkvgo.ts`](../web/mkvgo.ts)**
is the typed wrapper around it (copy the file into your project - it has no
dependencies). Every method returns a Promise and every error is a rejection.

| Method | Input | Result |
|---|---|---|
| `probe(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (head-only, any size) | metadata object - same shape as the CLI `-json` output, plus `format: "mkv"\|"mp4"`; every derived track string is a key (`codec_long_name`, `channel_layout`, aspect ratios, colour names, `resolved_language`, `effective_sample_rate`, and `hdr_format`: `dolby-vision`\|`hdr10`\|`hlg`\|`sdr`) |
| `remuxToMP4(input, opts?)` | `Uint8Array` (MKV/WebM) | `{ data: Uint8Array, droppedTracks }` |
| `remuxFromMP4(input, opts?)` | `Uint8Array` (MP4/MOV) | `{ data, droppedTracks }` (MKV) |
| `remuxToWebM(input)` | `Uint8Array` (MKV) | `{ data, droppedTracks }` (VP8/VP9/AV1 + Opus/Vorbis only) |
| `remuxToHLS(input, opts?)` | `Uint8Array` (MKV/WebM) | `{ files: {name → Uint8Array}, droppedTracks }` - `master.m3u8`, `playlist.m3u8`, `init.mp4`, `seg*.m4s`, subtitle renditions |
| `openHLS(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (ranged reads - no size limit) | on-demand handle: `{ numSegments, resources, resource(name) → {data, contentType}, segment(n), close() }` |
| `openABR(inputs, opts?)` | array of `Uint8Array`/`Blob`/`File` - pre-encoded quality variants, best first | on-demand ABR handle: `{ numVariants, resources, resource(name) → {data, contentType}, close() }` - `resource("master.m3u8")` or `resource("v2/seg00007.m4s")` |
| `openConcat(inputs, opts?)` | array of `Uint8Array`/`Blob`/`File` - sources in playback order | on-demand concatenated handle: `{ numParts, resources, resource(name) → {data, contentType}, close() }` - one continuous HLS session over several sources, `resource("master.m3u8")` or `resource("p1/seg00007.m4s")` |
| `extractSubtitleVTT(input, trackId)` | `Uint8Array` (MKV or MP4) | WebVTT `string` |
| `analyze(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (ranged reads - full block-header walk, not head-only) | `AnalyzeReport` - frame/keyframe counts, bitrate, GOP spans (plus the widest keyframe gap in TIME and where it opens - a stretch the frames are missing from is invisible to the frame-count GOP), duration reconciliation |
| `playability(input, target?, opts?)` | `Uint8Array` **or `Blob`/`File`** (head-only) | `PlayabilityReport` - per-track and overall verdict (`"direct-play"`\|`"remux"`\|`"transcode"`) against `target` (default `"mse-generic"`) |
| `ladder(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (head-only) | `Rung[]` - recommended ABR ladder capped at the source resolution/bitrate |
| `ingest(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (head-only, unless `opts.analyze`) | `ServingPlan` - one-call onboarding decision: strategy, seek-index check, ladder when transcode is needed; read-only (never reindexes) |
| `fingerprint(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (ranged reads - full payload read, not head-only) | `FingerprintReport` - container-independent content identity (per-track + whole-file digests) |
| `mapDamage(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (ranged reads - full tolerant walk, writes nothing) | `SalvageReport` - the damage map a repair would produce: repaired/lost ranges with byte offsets and approximate times; `opts.cleanCut` accounts for keyframe-aligned resume. The twin of `mkvgo salvage --dry-run`; the repair operations themselves are not in wasm (browser inputs are read-only) |
| `cueHealth(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (head-only, no cluster walk; `diagnose` adds the bounded hole probe) | `CueHealthReport` - can the index seek video? Spots a seek-broken index (non-empty but with no video cue) and one too coarse to land near its target, in milliseconds, with the remedy - the reason says where the hole is, the tail is measured to the picture's own end (statistics `DURATION` tag) rather than the declared duration, and `video_shortfall_ms` flags picture missing from the stream, which no index can cue. Cues on other tracks are counted, never held against a file: seeking never uses them |
| `diagnose(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (routed by the first bytes; MKV head-mostly - the tolerant walk runs only when the declared and real sizes disagree; MP4 head-only) | `Diagnosis` - one-call triage with a remedy per finding, same shape for both containers. MKV: index health + per-track audio start delays + size coherence. MP4: box-layout truncation, missing moov, trailing junk, edit-list audio delays |
| `openForensic(input, opts?)` | `Uint8Array` **or `Blob`/`File`** | single-source A/B watermark handle: `{ numSegments, masterPlaylist, mediaPlaylist, init, segment(n, fromB), segmentForPattern(n, pattern), distinct(n), close() }` - variant B derives from the one source by dropping a disposable H.264 frame per segment, timing-compensated (shared manifest); `distinct(n)` says whether segment n carries a bit |
| `version()` | - | version `string` |

Probe options: `{ keyframes?, bitrate?, inbandColour? }`. Remux options:
`{ fastStart?, skipUnsupported?, flattenSubs?, nativeWebVTT?,
mp3ContainerDelay?, contentHashes?, segmentSeconds?, keepTracks?, subOffsetMs?,
synthesizeIndex?, audioShiftMs?, windowCacheBytes? }`
- the same semantics as the CLI flags ([cli.md](cli.md)). `keepTracks` (an
array of track IDs) is the Virtual Edit Layer: `openHLS(file, { keepTracks: [1,
2] })` serves a "VF only" version from one file, no copy. `synthesizeIndex`
(openHLS) serves a no-Cue source by walking it once and synthesizing the index
in memory; `audioShiftMs` (`{trackNumber: ms}`, positive = presented earlier)
cancels an A/V desync in the served segments via the init's edit list alone -
both are serving-side repairs on a read-only input, nothing written.
`windowCacheBytes` (openHLS/openABR) bounds what a plan holds for the renditions
of a window nobody has collected. A player asks for the video and the audio of
the same instant, and one walk of the source builds both - their bytes are
interleaved, so reading one rendition's window reads them all - which is why the
second request costs **no read at all**. Over a `Blob`/`File`, where every read is
a range request the browser has to serve, that halves the traffic: a viewer reads
the source once instead of twice. Omit it and the plan sizes itself from the
source; a negative value turns the sharing off.
`openHLS`/`openABR`
additionally accept an `encrypt` option (AES-128 whole-segment) or a `cenc`
option (Common Encryption); `openConcat`
additionally accepts `keepLangs`. See below for both. `analyze`/`playability`/
`ladder` take just `{ signal? }` (abort only) - see [Stream analysis,
playability and ABR ladder](#stream-analysis-playability-and-abr-ladder).

Input format is sniffed from the first bytes (EBML magic vs ISO-BMFF box), not
from a file name.

## Stream analysis, playability and ABR ladder

Three metadata-only additions alongside probe - no upload, everything runs
client-side.

**`analyze(input, opts?)`** is a structural, no-decode stream-statistics pass:
exact frame/keyframe counts (lacing expanded - a laced audio block's frames are
counted individually), byte totals, average/peak bitrate, GOP spans (video)
and a duration reconciliation (the container's declared duration vs. the true
end-of-track timecode), all read from Matroska/WebM block headers - never a
decoded sample. Unlike `probe`, this needs a FULL walk of the file (every
block header, not just the head), so a `Blob`/`File` is read through ranged
slices to stay memory-bounded rather than head-only.

```js
const report = await MkvGo.analyze(file)   // File/Blob: ranged reads, full walk
console.log(report.duration_ms, report.overall_bitrate_bps, report.warnings)
for (const t of report.tracks)
  console.log(t.track_id, t.type, t.frames, t.keyframes, t.avg_bitrate_bps)
```

**`playability(input, target?, opts?)`** decides, from head-only metadata
alone (codec, profile, level, resolution, pixel format/bit depth,
colour/HDR/Dolby Vision, audio channels/sample rate - no block walk, no
decode), whether a file plays on `target` as-is (`"direct-play"`), needs only
a container remux (`"remux"`, with the cheapest container that would work in
`RemuxContainer`), or needs a real transcode (`"transcode"`) - both per track
and overall (the worst of any track). `target` defaults to `"mse-generic"`.
Built-in target names: `"safari"`, `"chrome"` (alias `"chromium-generic"`,
shared verbatim by `"brave"`, `"opera"`, `"vivaldi"`, `"samsung-internet"` -
same Chromium media pipeline), `"edge"` (the one Chromium browser with native
HEVC), `"firefox"`, `"chromecast-gen3"`, `"mse-generic"` (a conservative
H.264 + AAC baseline for a generic MSE player). An unrecognised target name
rejects the returned Promise.

```js
const p = await MkvGo.playability(file, 'safari')
console.log(p.OverallVerdict, p.RemuxContainer)         // e.g. "remux", "mp4"
for (const t of p.Tracks) console.log(t.TrackID, t.Type, t.Verdict, t.Reasons)
```

**`ladder(input, opts?)`** recommends a sensible ABR ladder (resolution +
bitrate rungs, tallest first) from the source's video track, head-only: it
never upscales (no rung wider/taller than the source) and never recommends a
bitrate above the source's own. This is guidance, not a guarantee - mkvgo
never transcodes, so the actual encode is always an external step.

```js
const rungs = await MkvGo.ladder(file)
for (const r of rungs) console.log(r.Label, `${r.Width}x${r.Height}`, `${r.BitrateKbps}kbps`)
```

## Onboarding a file (`ingest`)

**`ingest(input, opts?)`** is the one-call onboarding decision: it composes
`playability`, `ladder` and a seek-index check into a single `ServingPlan` -
`Strategy` is `"direct-play"` (serve the source as-is), `"remux-hls"` (package
on-demand HLS, no transcode - every track's codec is kept) or `"transcode"`
(at least one track needs a real re-encode; `Ladder` carries the recommended
rungs for an external encoder). `Reasons` is a short, ordered, human-readable
trail of every decision it made.

```js
const plan = await MkvGo.ingest(file, { target: 'safari' })
console.log(plan.Strategy, plan.SourceContainer, plan.Reasons)
if (plan.Strategy === 'remux-hls' && plan.NeedsReindex) {
  // no head-discoverable Cues index yet - see below
}
```

`opts.target` defaults to `"mse-generic"` (same target names as
`playability`); an unrecognised name rejects. `opts.analyze` also runs
`analyze` and attaches its report as `plan.Analysis` (forcing a full
block-header walk instead of head-only), regardless of the decided strategy.

**This binding is read-only: `Reindex` is always `false`.** When a
`"remux-hls"` decision finds no head-discoverable seek index, the plan sets
`NeedsReindex: true` and stops there - it never performs the in-place patch
itself, because a browser `MemFS`/`Blob` has nothing durable to write the
result back to. In the browser that only ever matters for a `File` picked
from local disk (nothing else gets ingested this way), so the practical
answer is: fall back to `remuxToHLS`/`openHLS` as usual, which build the
seek index they need on the fly without touching the source. A server or the
CLI - which own real files - run `ops.Ingest(Reindex: true)` (library) or the
`ingest` CLI command for the repairing path.

## Content identity for dedup (`fingerprint`)

**`fingerprint(input, opts?)`** computes a container-independent content
identity: a per-track SHA-256 over that track's frame payloads in decode
order, plus a `presentation` hash over all of them - the same
audio/video/subtitle content produces the same `presentation` hash whether
the source is Matroska or WebM, independent of track order or container
metadata, so a client-side media library can detect that two files are
re-muxes of the same content (different title, muxing app, or track order)
without a byte-for-byte comparison of the containers themselves.

```js
const fp = await MkvGo.fingerprint(file)
console.log(fp.presentation)                 // stable hex sha256, whole-file identity
for (const t of fp.tracks) console.log(t.track_id, t.type, t.codec, t.sha256)
```

Unlike `playability`/`ladder`, this is a **FULL read** - every frame payload
is read and hashed, like `analyze` - so a `Blob`/`File` is read through
ranged slices to stay memory-bounded rather than head-only, but the cost is
proportional to the media volume, not just the block-header count.
Matroska/WebM sources only for now (MP4 support is a follow-up).

## Playing a local file with bounded memory

```js
const plan = await MkvGo.openHLS(file, { segmentSeconds: 6 })   // File from an <input>
const ms = new MediaSource()
video.src = URL.createObjectURL(ms)
ms.addEventListener('sourceopen', async () => {
  const codecs = /CODECS="([^"]+)"/.exec(new TextDecoder().decode((await plan.resource('master.m3u8')).data))[1]
  const sb = ms.addSourceBuffer(`video/mp4; codecs="${codecs}"`)
  const append = (b) => new Promise((r) => { sb.addEventListener('updateend', r, { once: true }); sb.appendBuffer(b) })
  await append((await plan.resource('init.mp4')).data)
  for (let n = 0; n < plan.numSegments; n++) await append(await plan.segment(n))
  ms.endOfStream()
})
```

`plan.resources` lists every name a player could request - the HLS master and
the DASH `manifest.mpd` (same CMAF segments, two manifests), per-rendition
playlists/init/segments (video: `init.mp4`/`seg*.m4s`; audio track k:
`init_ak.mp4`/`seg_ak_*.m4s` - demuxed, so multi-audio selection works), and
one WebVTT rendition per text subtitle track (`sub1.m3u8` / `sub1.vtt`);
cover art and global tags ride on the video init segment. The source must
carry a Cues index (real muxers always write one).

## Virtual subtitle resync (`subOffsetMs`)

A viewer's subtitle drifts out of sync; `subOffsetMs` shifts every WebVTT cue
by that many milliseconds (negative allowed), with **no file rewritten** -
re-open with a new offset and the same source serves a different sync
instantly:

```js
const synced = await MkvGo.openHLS(file, { segmentSeconds: 6, subOffsetMs: -350 })   // subtitles 350ms earlier
```

A cue whose shifted end lands at or before 0 is dropped; a cue straddling 0 is
clamped to start at 0. Only the subtitle WebVTT renditions shift - video/audio
segments are byte-identical to a plan opened without the option. `subOffsetMs`
is also honoured by `openABR` and `openConcat`, and by the eager
`remuxToHLS` (same option object).

## AES-128 whole-segment HLS (`encrypt`)

`openHLS`/`openABR` accept an `encrypt` option for AES-128 whole-segment
encryption (RFC 8216): every media segment is encrypted as one AES-CBC blob and
the playlists carry an `EXT-X-KEY` line pointing at `keyURI`. This is the WASM
counterpart of the CLI `--aes-key`/`--aes-key-uri` flags and
`mp4.Options.Encrypt`. It is simpler than CENC (no per-sample subsamples, no
EME, any codec) and any HLS client that can fetch the key plays it.

```js
const plan = await MkvGo.openHLS(file, {
  segmentSeconds: 6,
  encrypt: {
    key: key16,                     // 16-byte Uint8Array, never written to the output
    keyURI: 'https://api.example.com/key',
    // iv: iv16,                    // optional; unset = each segment's media sequence number
  },
})
```

Set at most one of `encrypt` or `cenc`; setting both rejects the Promise (they
are different encryption schemes). A bad key length surfaces the same error the
CLI's `--aes-key` flag does. AES-128 is HLS-only - unlike CENC it produces no
DASH manifest.

**Key rotation** (forward secrecy): rotate the key across the presentation so a
captured key decrypts only its own period. Set `rotateEverySegments` and a
`keys` array instead of a single `key`; the media playlist then carries a fresh
`EXT-X-KEY` at each boundary and each segment is encrypted with its period's
key. The schedule is a pure function of the segment index, so an on-demand plan
and the full write agree byte for byte.

```js
const plan = await MkvGo.openHLS(file, {
  segmentSeconds: 6,
  encrypt: {
    rotateEverySegments: 10,        // new key every 10 segments, cycling through keys[]
    keys: [
      { key: keyA, keyURI: 'https://api.example.com/key/a' },
      { key: keyB, keyURI: 'https://api.example.com/key/b' },
    ],
  },
})
```

The CLI counterpart is `--aes-rotate-segments N` with comma-separated
`--aes-key`/`--aes-key-uri` lists.

## Common Encryption (`cenc`)

`openHLS`/`openABR` accept a `cenc` option to package every media segment
under Common Encryption (ISO/IEC 23001-7) - the sample-level encryption an
EME-capable player's own DRM path (Widevine/PlayReady/FairPlay CDM) consumes.
This is **packaging only**: no license server, no DRM handshake - the caller
supplies the key and its delivery (`keyURI`).

```js
const plan = await MkvGo.openHLS(file, {
  segmentSeconds: 6,
  cenc: {
    scheme: 'cenc',                 // or 'cbcs' (AES-CBC, 1:9 pattern on video)
    key: key16,                     // 16-byte Uint8Array, never written to the output
    keyId: kid16,                   // 16-byte Uint8Array (tenc default_KID)
    iv: iv8,                        // 8 or 16 bytes for "cenc", 16 for "cbcs"
    keyURI: 'https://api.example.com/key',
  },
})
```

Video may be H.264, HEVC, AV1 or VP9 (subsample encryption keeps each codec's
decoder-visible header bytes clear and protects the coded data, verified in a
real Clear Key player); a bad key/keyId/IV length or a frame construct the
AV1/VP9 parsers do not yet cover rejects the returned Promise with the same
error the CLI's `--cenc-*` flags surface. Unlike `Options.Encrypt` (AES-128,
HLS-only), a CENC presentation still gets a DASH manifest (with a
`ContentProtection` element) - the AV1/VP9 path exists precisely to give all-AV1
and iOS/DASH audiences protected playback.
**PSSH boxes are not exposed by this wasm build** (v1) - build them
server-side and inject them into the init segment yourself if a DRM system
needs one; see [library.md](library.md#common-encryption-cenc) for the full
detail (clear/protected split, IV derivation).

## Forensic A/B session watermarking (`openWatermark`)

`openWatermark(a, b, opts?)` serves two GOP-aligned encodes of one title as ONE
HLS presentation whose per-segment bytes are drawn from variant A or B by a
per-viewer bit pattern. A leaked copy then carries a binary signature
identifying the session. No re-encode: the manifest is shared across every
viewer (A and B are aligned), and only the per-segment A/B choice differs.

```js
const wm = await MkvGo.openWatermark(titleA, titleB, { segmentSeconds: 6 })
// serve the shared manifest to every viewer:
const playlist = wm.mediaPlaylist            // Uint8Array, identical for all sessions
// per session, route each segment by the viewer's code bit:
const seg = await wm.segment(n, sessionBit)                 // A (false) or B (true)
const seg2 = await wm.segmentForPattern(n, sessionCodeBytes) // bit n of the code
```

The two encodes must be spliceable (identical init, same segment count and
durations) or `openWatermark` rejects them. mkvgo supplies the mechanism; the
**code assignment** - which session gets which pattern, and collusion-resistant
codes - is the caller's policy, applied when serving each segment. Not combined
with `encrypt`/`cenc` in this version.

## Gapless multi-file sessions (`openConcat`)

`openConcat` plays several sources - e.g. consecutive episodes - as ONE
continuous HLS session: a single `master.m3u8`/`playlist.m3u8`/`audioN.m3u8`,
so a player never reloads and never sees a session boundary. Nothing is
pre-generated: each part packages into its own `p{k}/` resources on demand,
exactly as `openHLS` would standalone, and the top-level playlists stitch them
together with `EXT-X-DISCONTINUITY`.

```js
// binge-play: one continuous <video> across several episode files, no reload
const plan = await MkvGo.openConcat(episodeFiles, { segmentSeconds: 6 })
const ms = new MediaSource()
video.src = URL.createObjectURL(ms)
ms.addEventListener('sourceopen', async () => {
  const codecs = /CODECS="([^"]+)"/.exec(new TextDecoder().decode((await plan.resource('master.m3u8')).data))[1]
  const sb = ms.addSourceBuffer(`video/mp4; codecs="${codecs}"`)
  const append = (b) => new Promise((r) => { sb.addEventListener('updateend', r, { once: true }); sb.appendBuffer(b) })
  for (let k = 0; k < plan.numParts; k++) {
    await append((await plan.resource(`p${k}/init.mp4`)).data)
    const segs = plan.resources.filter((n) => new RegExp(`^p${k}/seg\\d+\\.m4s$`).test(n)).sort()
    for (const name of segs) await append((await plan.resource(name)).data)
  }
  ms.endOfStream()
})
```

Sources must be compatible: same video codec family, same kept-audio layout
(count, codec, language, order) - checked from track metadata alone before any
part is packaged, so an incompatible set fails fast. Subtitles ride along only
when every part's rendition layout aligns (count/language/name/forced);
otherwise they are dropped from the concatenated presentation, and the
video/audio concatenation still plays. `opts.keepLangs` (an array of language
codes, e.g. `['fre']`) resolves a language-based track subset from the
**first** source's metadata - video is always kept - the wasm equivalent of
the CLI's `--keep-lang`; ignored when `keepTracks` is set.

## In-browser HLS origin (Service Worker)

Instead of driving MediaSource by hand, a Service Worker can make the WASM an
**HLS origin**: it intercepts requests under a virtual path and answers them
from `openHLS`/`openABR`, so a plain `<video>` (Safari) or hls.js (elsewhere)
streams a local file - even one far larger than memory - with no server and no
upload. The worker is just the fetch router; the `resource(name)` work is all
WASM.

```js
// mkvgo-sw.js (Service Worker)
importScripts('/wasm_exec.js')
const ready = (async () => {
  const go = new Go()
  const { instance } = await WebAssembly.instantiateStreaming(fetch('/mkvgo.wasm'), go.importObject)
  go.run(instance)
  while (!self.MkvGo) await new Promise(r => setTimeout(r, 5))
})()
const plans = new Map()

self.addEventListener('message', (e) => {          // page hands over the File(s)
  const { id, inputs, opts } = e.data
  e.waitUntil(ready.then(async () => {
    plans.set(id, inputs.length > 1 ? await MkvGo.openABR(inputs, opts) : await MkvGo.openHLS(inputs[0], opts))
    e.source.postMessage({ type: 'ready', id })
  }))
})

self.addEventListener('fetch', (e) => {            // serve __mkvgo__/<id>/<resource>
  const m = new URL(e.request.url).pathname.match(/\/__mkvgo__\/([^/]+)\/(.+)$/)
  if (!m) return
  e.respondWith(ready.then(async () => {
    const plan = plans.get(m[1])
    if (!plan) return new Response('no session', { status: 404 })
    const { data, contentType, sha256 } = await plan.resource(m[2])
    // sha256 is a stable content ETag (deterministic output) - HTTP caching for free.
    return new Response(data, { headers: { 'Content-Type': contentType, 'ETag': `"${sha256}"` } })
  }))
})
```

```js
// page: register, hand over the File(s), point the player at the virtual master
await navigator.serviceWorker.register('/mkvgo-sw.js')
await navigator.serviceWorker.ready
const id = 'sess1'
navigator.serviceWorker.controller.postMessage({ type: 'open', id, inputs: files, opts: { segmentSeconds: 6 } })
// on the { type:'ready', id } reply:
video.src = `/__mkvgo__/${id}/master.m3u8`   // Safari; else hls.js.loadSource(...)
```

A complete runnable page is [`web/example/hls-sw.html`](../web/example/hls-sw.html)
with [`web/example/mkvgo-sw.js`](../web/example/mkvgo-sw.js). (A Service Worker's
lifecycle can evict it between events; a production origin re-opens the plan on
demand rather than holding it in a `Map`.)

## Quickstart (browser, no bundler)

```html
<script src="/wasm_exec.js"></script>
<script type="module">
  const go = new Go()
  const { instance } = await WebAssembly.instantiateStreaming(fetch('/mkvgo.wasm'), go.importObject)
  go.run(instance)
  while (!globalThis.MkvGo) await new Promise(r => setTimeout(r, 5))

  document.querySelector('input[type=file]').onchange = async (e) => {
    const probe = await MkvGo.probe(e.target.files[0])   // head-only, any size
    console.log(probe.info.title, probe.tracks)
  }
</script>
```

The [runnable demo](../web/example/index.html) (`make wasm`, then
`python3 -m http.server` from the repo root) adds drag-and-drop probing, MP4
download, and MSE playback of the HLS output.

## With the TypeScript wrapper

```ts
import { loadMkvGo } from './mkvgo'   // copy web/mkvgo.ts into your project

const mkvgo = await loadMkvGo({ wasmUrl: '/mkvgo.wasm', wasmExecUrl: '/wasm_exec.js' })

const probe = await mkvgo.probe(file)                    // File → head-only
const { data } = await mkvgo.remuxToMP4(bytes, { fastStart: true })
```

## Aborting and streaming

Every method takes `{ signal?: AbortSignal }` in its options - an abort
cancels the in-flight Go work (probe reads, remux, segment builds), which is
what a React effect cleanup wants. `hlsSegmentStream(plan)` (in `mkvgo.ts`)
exposes the video rendition as a progressive `ReadableStream<Uint8Array>`  - 
init then each segment, built as the consumer pulls; cancelling the stream
aborts the current build.

## React

**[`web/react.ts`](../web/react.ts)** ships ready-made hooks (copy both files
into your project):

- `useMkvGo(loadOptions)` - module loading, null until ready;
- `useProbe(mkvgo, file)` - head-only probe with automatic abort on change;
- `useHLSPlayer(mkvgo, videoRef, file)` - plays a local MKV File in a
  `<video>` through MSE: on-demand demuxed segments from ranged reads
  (bounded memory, any size), video + audio SourceBuffers, cleanup on
  unmount.

```tsx
function Player({ file }: { file: File }) {
  const mkvgo = useMkvGo({ wasmUrl: '/mkvgo.wasm', wasmExecUrl: '/wasm_exec.js' })
  const videoRef = useRef<HTMLVideoElement>(null)
  const { probe } = useProbe(mkvgo, file)
  const { state, error } = useHLSPlayer(mkvgo, videoRef, file)
  return <div>
    <video ref={videoRef} controls />
    <p>{probe?.info.title} - {state}{error ? `: ${error.message}` : ''}</p>
  </div>
}
```

Or hand-rolled - a hook owning the module and a probe component:

```tsx
// useMkvGo.ts
import { useEffect, useState } from 'react'
import { loadMkvGo, type MkvGoApi } from './mkvgo'

export function useMkvGo(): MkvGoApi | null {
  const [api, setApi] = useState<MkvGoApi | null>(null)
  useEffect(() => {
    loadMkvGo({ wasmUrl: '/mkvgo.wasm', wasmExecUrl: '/wasm_exec.js' }).then(setApi)
  }, [])
  return api
}
```

```tsx
// MediaInspector.tsx
import { useState } from 'react'
import { useMkvGo } from './useMkvGo'
import type { ProbeResult } from './mkvgo'

export function MediaInspector() {
  const mkvgo = useMkvGo()
  const [probe, setProbe] = useState<ProbeResult | null>(null)

  return (
    <div>
      <input type="file" accept=".mkv,.webm,.mp4,.mov" disabled={!mkvgo}
        onChange={async (e) => {
          const file = e.target.files?.[0]
          if (file && mkvgo) setProbe(await mkvgo.probe(file))   // no size limit
        }} />
      {probe && (
        <ul>
          {probe.tracks.map((t) => (
            <li key={t.id}>
              #{t.id} {t.type} - {t.codec_long_name ?? t.codec}
              {t.width ? ` ${t.width}×${t.height}` : ''}
              {t.channel_layout ? ` ${t.channel_layout}` : ''}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
```

Remuxing a dropped file to a downloadable MP4:

```tsx
const onRemux = async (file: File) => {
  const bytes = new Uint8Array(await file.arrayBuffer())
  const { data, droppedTracks } = await mkvgo.remuxToMP4(bytes, { fastStart: true, skipUnsupported: true })
  droppedTracks.forEach((d) => console.warn(`dropped #${d.ID} (${d.Codec}): ${d.Reason}`))
  const url = URL.createObjectURL(new Blob([data], { type: 'video/mp4' }))
  Object.assign(document.createElement('a'), { href: url, download: 'out.mp4' }).click()
}
```

## Vue 3

**[`web/vue.ts`](../web/vue.ts)** ships ready-made composables mirroring the
React hooks (copy it and `mkvgo.ts` into your project): `useMkvGo`, `useProbe`
(auto-abort), and `useHLSPlayer` (MSE playback of a local File, bounded memory).

```vue
<!-- MediaInspector.vue -->
<script setup lang="ts">
import { ref } from 'vue'
import { useMkvGo, useProbe } from './vue'

const mkvgo = useMkvGo({ wasmUrl: '/mkvgo.wasm', wasmExecUrl: '/wasm_exec.js' })
const file = ref<File | null>(null)
const { probe } = useProbe(file)
</script>

<template>
  <input type="file" :disabled="!mkvgo" @change="e => file = (e.target as HTMLInputElement).files?.[0] ?? null" />
  <ul v-if="probe"><li v-for="t in probe.tracks" :key="t.id">#{{ t.id }} {{ t.type }} - {{ t.codec_long_name ?? t.codec }}</li></ul>
</template>
```

Or hand-rolled, with just the module loader:

```ts
// useMkvGo.ts
import { shallowRef } from 'vue'
import { loadMkvGo, type MkvGoApi } from './mkvgo'

const api = shallowRef<MkvGoApi | null>(null)
loadMkvGo({ wasmUrl: '/mkvgo.wasm', wasmExecUrl: '/wasm_exec.js' }).then((m) => (api.value = m))

export function useMkvGo() {
  return api   // shared instance; null until loaded
}
```

## Node

```js
import fs from 'node:fs'
import { createRequire } from 'node:module'
createRequire(import.meta.url)('./dist/wasm/wasm_exec.js')   // defines globalThis.Go

const go = new Go()
const { instance } = await WebAssembly.instantiate(fs.readFileSync('./dist/wasm/mkvgo.wasm'), go.importObject)
go.run(instance)
while (!globalThis.MkvGo) await new Promise((r) => setTimeout(r, 5))

const probe = await MkvGo.probe(new Uint8Array(fs.readFileSync('movie.mkv')))
```

(In Node the native `mkvgo` binary is normally the better tool; the wasm build
matters when the same code must run in both browser and server bundles.
`scripts/wasm_smoke.mjs` is a complete working example.)

## Off the main thread

Remuxing a large file blocks for its duration (a fraction of a second per
hundred MB, but still). Run it in a **web worker**: the loader works unchanged
in a worker (`importScripts('/wasm_exec.js')` or a module worker), and
`Uint8Array` inputs/outputs are transferable.

## Limits

- **Remux needs the whole file in memory** (input + output simultaneously  - 
  wasm32 addresses 4 GB; in practice keep inputs under ~1.5 GB). `probe`,
  `playability`, `ladder` and `ingest` (unless `opts.analyze` is set) accept a
  `Blob`/`File` and have **no size limit** - they read head-only. `analyze`
  and `fingerprint` also accept a `Blob`/`File` (they walk every block header
  / read every payload respectively, so the input itself stays
  memory-bounded even on a huge file, unlike remux).
- **`ingest` never reindexes in wasm** - it is a read-only decision client;
  see [Onboarding a file](#onboarding-a-file-ingest). `fingerprint` is
  Matroska/WebM only for now.
- **No transcoding**, by design: only the codecs the native remuxers support
  are carried (see [cli.md](cli.md)); unsupported tracks fail or are dropped
  with `skipUnsupported`/reported in `droppedTracks`.
- One in-flight module instance; calls are safe to issue concurrently (each
  runs on its own goroutine), but they share one wasm heap.
