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
