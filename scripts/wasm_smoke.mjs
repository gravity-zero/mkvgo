// Node smoke test for the WebAssembly build (run by `make wasm-smoke`).
// Exercises the real dist/wasm/mkvgo.wasm artifact end to end: probe on
// Uint8Array / Blob / MP4, MKV -> MP4 -> MKV round-trip, HLS packaging, and
// error surfacing. Fails loudly (non-zero exit) on any mismatch.
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')

// wasm_exec.js is a classic script assigning globalThis.Go; import() of a
// non-module still runs it in module scope, so evaluate it globally instead.
const { createRequire } = await import('node:module')
createRequire(import.meta.url)(path.join(root, 'dist/wasm/wasm_exec.js'))

const go = new Go()
const wasm = fs.readFileSync(path.join(root, 'dist/wasm/mkvgo.wasm'))
const { instance } = await WebAssembly.instantiate(wasm, go.importObject)
go.run(instance) // resolves only on runtime exit; MkvGo appears once main registers it
while (!globalThis.MkvGo) await new Promise((r) => setTimeout(r, 10))
const MkvGo = globalThis.MkvGo

let failures = 0
const check = (cond, label) => {
  if (cond) console.log(`  ok: ${label}`)
  else { console.error(`  FAIL: ${label}`); failures++ }
}

console.log(`MkvGo version: ${MkvGo.version()}`)

// --- probe (Uint8Array, MKV) ---
const mkvBytes = new Uint8Array(fs.readFileSync(path.join(root, 'internal/testdata/sample.mkv')))
const probe = await MkvGo.probe(mkvBytes, { keyframes: true })
check(probe.format === 'mkv', `probe format mkv (got ${probe.format})`)
check(Array.isArray(probe.tracks) && probe.tracks.length >= 1, `probe tracks (${probe.tracks?.length})`)
check(probe.tracks[0].codec_long_name?.length > 0, `derived codec_long_name (${probe.tracks[0].codec_long_name})`)

// --- probe (Blob) — the ranged-read path used for large browser files ---
const probeBlob = await MkvGo.probe(new Blob([mkvBytes]))
check(probeBlob.format === 'mkv' && probeBlob.tracks.length === probe.tracks.length,
  'probe(Blob) matches probe(Uint8Array)')

// --- probe (MP4/QuickTime) ---
const movBytes = new Uint8Array(fs.readFileSync(path.join(root, 'internal/testdata/quicktime.mov')))
const probeMov = await MkvGo.probe(movBytes)
check(probeMov.format === 'mp4', `probe .mov format mp4 (got ${probeMov.format})`)

// --- MKV -> MP4 -> MKV round-trip ---
const mp4 = await MkvGo.remuxToMP4(mkvBytes, { fastStart: true })
check(mp4.data instanceof Uint8Array && mp4.data.length > 0, `remuxToMP4 output (${mp4.data.length} bytes)`)
check(new TextDecoder().decode(mp4.data.slice(4, 8)) === 'ftyp', 'MP4 starts with ftyp')
const back = await MkvGo.remuxFromMP4(mp4.data)
check(back.data[0] === 0x1a && back.data[1] === 0x45 && back.data[2] === 0xdf && back.data[3] === 0xa3,
  'remuxFromMP4 output is EBML')

// --- HLS packaging ---
const hls = await MkvGo.remuxToHLS(mkvBytes, { segmentSeconds: 0.5 })
for (const f of ['master.m3u8', 'playlist.m3u8', 'init.mp4', 'seg00001.m4s'])
  check(hls.files[f]?.length > 0, `HLS emits ${f}`)
const playlist = new TextDecoder().decode(hls.files['playlist.m3u8'])
check(playlist.includes('#EXT-X-MAP:URI="init.mp4"') && playlist.includes('#EXT-X-ENDLIST'), 'playlist structure')

// --- on-demand HLS: handle over bytes and over Blob, byte-identical to the full pass ---
const plan = await MkvGo.openHLS(mkvBytes, { segmentSeconds: 0.5 })
check(plan.numSegments > 0 && plan.resources.includes('master.m3u8'), `openHLS resources (${plan.resources.length})`)
const initRes = await plan.resource('init.mp4')
check(initRes.contentType === 'video/mp4' && Buffer.compare(initRes.data, hls.files['init.mp4']) === 0,
  'on-demand init byte-identical to remuxToHLS')
const seg1 = await plan.segment(0)
check(Buffer.compare(seg1, hls.files['seg00001.m4s']) === 0, 'on-demand segment 1 byte-identical')
const planBlob = await MkvGo.openHLS(new Blob([mkvBytes]), { segmentSeconds: 0.5 })
const seg1Blob = await planBlob.segment(0)
check(Buffer.compare(seg1Blob, seg1) === 0, 'openHLS(Blob) ranged reads produce the same segment')
await plan.resource('nope.bin')
  .then(() => check(false, 'unknown resource must reject'))
  .catch(() => check(true, 'unknown resource rejects'))
plan.close(); planBlob.close()

// --- on-demand ABR: one multi-variant plan over two pre-encoded qualities ---
const abr = await MkvGo.openABR([mkvBytes, mkvBytes], { segmentSeconds: 0.5 })
check(abr.numVariants === 2 && abr.resources.includes('master.m3u8'), `openABR ${abr.numVariants} variants`)
const abrMaster = new TextDecoder().decode((await abr.resource('master.m3u8')).data)
check((abrMaster.match(/EXT-X-STREAM-INF/g) || []).length === 2 && abrMaster.includes('TYPE=AUDIO'),
  'ABR master declares both variants and the shared audio group')
const v1seg = await abr.resource('v1/seg00001.m4s')
check(Buffer.compare(v1seg.data, hls.files['seg00001.m4s']) === 0,
  'ABR v1 segment byte-identical to the single-source full pass')
const v2init = await abr.resource('v2/init.mp4')
check(v2init.data.length > 0, 'ABR v2 (video-only) init served')
const abrBlob = await MkvGo.openABR([new Blob([mkvBytes]), new Blob([mkvBytes])], { segmentSeconds: 0.5 })
const v1segBlob = await abrBlob.resource('v1/seg00001.m4s')
check(Buffer.compare(v1segBlob.data, v1seg.data) === 0, 'openABR(Blob[]) ranged reads produce the same segment')
await abr.resource('v9/init.mp4')
  .then(() => check(false, 'out-of-range variant must reject'))
  .catch(() => check(true, 'out-of-range ABR variant rejects'))

// --- Service Worker serve contract (the routing the browser SW does): a
// virtual URL __mkvgo__/<id>/<resource> maps to plan.resource(resource) ---
const VIRTUAL = '__mkvgo__'
const parseVirtualURL = (pathname) => {
  const i = pathname.indexOf('/' + VIRTUAL + '/')
  if (i < 0) return null
  const rest = pathname.slice(i + VIRTUAL.length + 2)
  const slash = rest.indexOf('/')
  if (slash < 1) return null
  return { id: rest.slice(0, slash), name: rest.slice(slash + 1) }
}
const route = parseVirtualURL('/web/example/__mkvgo__/sess0/v2/seg00001.m4s')
check(route && route.id === 'sess0' && route.name === 'v2/seg00001.m4s', 'SW virtual URL routes to the resource name')
check(parseVirtualURL('/favicon.ico') === null, 'SW ignores non-mkvgo requests')
const served = await abr.resource(route.name)
const direct = await abr.resource('v2/seg00001.m4s')
check(Buffer.compare(served.data, direct.data) === 0 && served.contentType.length > 0,
  'SW serves the ABR resource the router resolved (bytes + Content-Type)')
// Deterministic content ETag: the resource's sha256 matches its bytes and is
// stable across calls — a Service Worker/server sets ETag from it directly.
const crypto = await import('node:crypto')
const want = crypto.createHash('sha256').update(served.data).digest('hex')
check(served.sha256 === want && served.sha256 === direct.sha256,
  'resource sha256 is the content hash (stable ETag)')
abr.close(); abrBlob.close()

// --- on-demand subtitles: the windowed WebVTT rendition over Blob ranged
// reads, byte-identical to the full pass (prefix scan, whole track, and the
// declarative resource surface a player actually hits) ---
const subBytes = new Uint8Array(fs.readFileSync(path.join(root, 'internal/testdata/sample_subs.mkv')))
const hlsSub = await MkvGo.remuxToHLS(subBytes, { segmentSeconds: 2 })
check(hlsSub.files['sub1.m3u8']?.length > 0 && hlsSub.files['sub1_00001.vtt']?.length > 0,
  'HLS emits the subtitle rendition')
const planSub = await MkvGo.openHLS(new Blob([subBytes]), { segmentSeconds: 2 })
check(planSub.resources.includes('sub1.m3u8') && planSub.resources.includes('sub1_00001.vtt'),
  'openHLS lists the windowed subtitle segments')
const w1 = await planSub.resource('sub1_00001.vtt')
check(w1.contentType === 'text/vtt' && Buffer.compare(w1.data, hlsSub.files['sub1_00001.vtt']) === 0,
  'windowed subtitle segment byte-identical over Blob ranged reads')
const lastSub = `sub1_${String(planSub.numSegments).padStart(5, '0')}.vtt`
const wLast = await planSub.resource(lastSub)
check(Buffer.compare(wLast.data, hlsSub.files[lastSub]) === 0,
  `out-of-order subtitle window byte-identical (${lastSub})`)
const wholeSub = await planSub.resource('sub1.vtt')
check(Buffer.compare(wholeSub.data, hlsSub.files['sub1.vtt']) === 0,
  'whole-track subtitle byte-identical')
planSub.close()

// --- MP4-source on-demand plan: sample-table backend, iframe playlist exposed ---
const planMp4 = await MkvGo.openHLS(mp4.data, { segmentSeconds: 0.5 })
check(planMp4.numSegments > 0 && planMp4.resources.includes('iframe.m3u8'),
  `openHLS(mp4) plans from the sample table (${planMp4.numSegments} segs)`)
const ifr = await planMp4.resource('iframe.m3u8')
check(new TextDecoder().decode(ifr.data).includes('#EXT-X-I-FRAMES-ONLY'), 'iframe playlist structure')
planMp4.close()

// --- AbortSignal: an aborted call rejects instead of running ---
const aborted = AbortSignal.abort()
await MkvGo.probe(new Blob([mkvBytes]), { signal: aborted })
  .then(() => check(false, 'aborted probe must reject'))
  .catch(() => check(true, 'aborted probe rejects'))
const planAb = await MkvGo.openHLS(mkvBytes, { segmentSeconds: 0.5 })
await planAb.segment(0, { signal: aborted })
  .then(() => check(false, 'aborted segment must reject'))
  .catch(() => check(true, 'aborted segment rejects'))
planAb.close()

// --- errors surface as rejections, not crashes ---
await MkvGo.probe(new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8]))
  .then(() => check(false, 'garbage probe must reject'))
  .catch((e) => check(/unrecognised container/.test(e.message), `garbage probe rejects (${e.message})`))
await MkvGo.remuxToWebM(mkvBytes) // h264/aac is not WebM-eligible
  .then(() => check(false, 'h264 remuxToWebM must reject'))
  .catch(() => check(true, 'h264 remuxToWebM rejects'))

if (failures) { console.error(`wasm smoke: ${failures} FAILURE(S)`); process.exit(1) }
console.log('wasm smoke: ALL PASS')
process.exit(0)
