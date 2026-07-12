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
{
  const v = probe.tracks.find((t) => t.type === 'video')
  check(v && v.sample_aspect_ratio?.includes(':') && v.display_aspect_ratio?.includes(':') &&
    (v.hdr_format === undefined || ['dolby-vision', 'hdr10', 'hlg', 'sdr'].includes(v.hdr_format)),
    `derived probe fields (sar=${v?.sample_aspect_ratio} dar=${v?.display_aspect_ratio} hdr=${v?.hdr_format ?? 'n/a'})`)
}

// --- probe (Blob) - the ranged-read path used for large browser files ---
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

// --- AES-128 whole-segment HLS (encrypt): the media playlist carries EXT-X-KEY ---
const aesKey = new Uint8Array(16).fill(7)
const planEnc = await MkvGo.openHLS(mkvBytes, {
  segmentSeconds: 0.5,
  encrypt: { key: aesKey, keyURI: 'https://example.test/k' },
})
const encPlaylist = new TextDecoder().decode((await planEnc.resource('playlist.m3u8')).data)
check(encPlaylist.includes('#EXT-X-KEY:METHOD=AES-128') && encPlaylist.includes('URI="https://example.test/k"'),
  'openHLS encrypt emits EXT-X-KEY in the media playlist')
const encSeg = await planEnc.segment(0)
check(encSeg.length > 0 && Buffer.compare(Buffer.from(encSeg), Buffer.from(seg1)) !== 0,
  'encrypted segment differs from the clear segment')
planEnc.close()

// --- AES-128 key rotation: the { rotateEverySegments, keys } shape is accepted
// and the first period's key is applied. (This fixture has a single video
// segment, so it cannot show a mid-playlist key change; the byte-level
// multi-period rotation is proven by the Go test TestHLSKeyRotation.) ---
const planRot = await MkvGo.openHLS(mkvBytes, {
  segmentSeconds: 0.5,
  encrypt: {
    rotateEverySegments: 1,
    keys: [
      { key: new Uint8Array(16).fill(1), keyURI: 'https://k/a' },
      { key: new Uint8Array(16).fill(2), keyURI: 'https://k/b' },
    ],
  },
})
const rotPlaylist = new TextDecoder().decode((await planRot.resource('playlist.m3u8')).data)
check(rotPlaylist.includes('#EXT-X-KEY:METHOD=AES-128,URI="https://k/a"'),
  'openHLS accepts a rotation schedule and applies the first period key')
planRot.close()

// --- forensic A/B watermarking: two aligned variants serve one shared manifest,
// per-segment bytes routed by an A/B bit. (Here a === b, so alignment trivially
// holds and the routing is exercised; real variants differ imperceptibly.) ---
const wm = await MkvGo.openWatermark(mkvBytes, mkvBytes, { segmentSeconds: 0.5 })
check(wm.numSegments > 0 && wm.mediaPlaylist.length > 0 && wm.init.length > 0, 'openWatermark shared manifest')
const segA = await wm.segment(0, false)
const segB = await wm.segment(0, true)
const segP = await wm.segmentForPattern(0, new Uint8Array([0x01])) // bit0 set -> B
check(segA.length > 0 && Buffer.compare(Buffer.from(segB), Buffer.from(segP)) === 0,
  'openWatermark routes segments by A/B bit and by pattern')
wm.close()

// --- single-source forensic: variant B derived from ONE source by dropping a
// disposable H.264 frame per segment (no second encode). Whether a given
// fixture segment carries a bit depends on its frames; both outcomes are valid
// and the contract differs: distinct -> B is strictly smaller, else B === A. ---
const fo = await MkvGo.openForensic(mkvBytes, { segmentSeconds: 0.5 })
check(fo.numSegments > 0 && fo.mediaPlaylist.length > 0 && fo.init.length > 0, 'openForensic shared manifest')
const foA = await fo.segment(0, false)
const foB = await fo.segment(0, true)
const foD = await fo.distinct(0)
check(typeof foD === 'boolean' &&
  (foD ? foB.length < foA.length : Buffer.compare(Buffer.from(foA), Buffer.from(foB)) === 0),
  `openForensic variant B ${foD ? 'drops one frame' : 'equals A (no disposable frame in segment 0)'}`)
const foP = await fo.segmentForPattern(0, new Uint8Array([0x01]))
check(Buffer.compare(Buffer.from(foB), Buffer.from(foP)) === 0, 'openForensic pattern routing selects B')
fo.close()

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
// stable across calls - a Service Worker/server sets ETag from it directly.
const crypto = await import('node:crypto')
const want = crypto.createHash('sha256').update(served.data).digest('hex')
check(served.sha256 === want && served.sha256 === direct.sha256,
  'resource sha256 is the content hash (stable ETag)')
abr.close(); abrBlob.close()

// --- on-demand concat: two identical sources played as one continuous session ---
const concat = await MkvGo.openConcat([mkvBytes, mkvBytes], { segmentSeconds: 0.5 })
check(concat.numParts === 2 && concat.resources.includes('master.m3u8') && concat.resources.length > 0,
  `openConcat ${concat.numParts} parts, ${concat.resources.length} resources`)
const concatMaster = new TextDecoder().decode((await concat.resource('master.m3u8')).data)
check(concatMaster.includes('#EXT-X-STREAM-INF'), 'concat master declares a rendition')
const p1Seg = await concat.resource('p1/seg00001.m4s')
check(p1Seg.data.length > 0, 'concat part 1 segment served')
concat.close()

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

// --- subOffsetMs: virtual per-session subtitle resync, media untouched ---
const planSub0 = await MkvGo.openHLS(subBytes, { segmentSeconds: 2, subOffsetMs: 0 })
const planSub5000 = await MkvGo.openHLS(subBytes, { segmentSeconds: 2, subOffsetMs: 5000 })
const vtt0 = (await planSub0.resource('sub1.vtt')).data
const vtt5000 = (await planSub5000.resource('sub1.vtt')).data
check(Buffer.compare(vtt0, vtt5000) !== 0, 'subOffsetMs shifts the subtitle VTT cue timestamps')
const vseg0 = await planSub0.segment(0)
const vseg5000 = await planSub5000.segment(0)
check(Buffer.compare(vseg0, vseg5000) === 0, 'subOffsetMs does not touch the video media segment')
planSub0.close(); planSub5000.close()

// --- MP4-source on-demand plan: sample-table backend, iframe playlist exposed ---
const planMp4 = await MkvGo.openHLS(mp4.data, { segmentSeconds: 0.5 })
check(planMp4.numSegments > 0 && planMp4.resources.includes('iframe.m3u8'),
  `openHLS(mp4) plans from the sample table (${planMp4.numSegments} segs)`)
const ifr = await planMp4.resource('iframe.m3u8')
check(new TextDecoder().decode(ifr.data).includes('#EXT-X-I-FRAMES-ONLY'), 'iframe playlist structure')
planMp4.close()

// --- analyze: no-decode stream stats (full block-header walk) ---
const analyzeReport = await MkvGo.analyze(mkvBytes)
check(Array.isArray(analyzeReport.tracks) && analyzeReport.tracks.length > 0,
  `analyze tracks (${analyzeReport.tracks?.length})`)
const analyzeVideo = analyzeReport.tracks.find((t) => t.type === 'video')
check(analyzeVideo && analyzeVideo.frames > 0 && analyzeVideo.keyframes > 0,
  `analyze video track frames=${analyzeVideo?.frames} keyframes=${analyzeVideo?.keyframes}`)
check(analyzeReport.duration_ms > 0, `analyze duration_ms (${analyzeReport.duration_ms})`)
const analyzeBlob = await MkvGo.analyze(new Blob([mkvBytes]))
check(analyzeBlob.duration_ms === analyzeReport.duration_ms && analyzeBlob.tracks.length === analyzeReport.tracks.length,
  'analyze(Blob) ranged reads match analyze(Uint8Array)')

// --- playability: verdict model against a target (default mse-generic, plus safari) ---
const playMse = await MkvGo.playability(mkvBytes, 'mse-generic')
check(['direct-play', 'remux', 'transcode'].includes(playMse.overall_verdict),
  `playability mse-generic overall verdict (${playMse.overall_verdict})`)
check(Array.isArray(playMse.tracks) && playMse.tracks.length > 0 &&
  playMse.tracks.every((t) => ['direct-play', 'remux', 'transcode'].includes(t.verdict)),
  'playability per-track verdicts')
const playSafari = await MkvGo.playability(mkvBytes, 'safari')
check(['direct-play', 'remux', 'transcode'].includes(playSafari.overall_verdict),
  `playability safari overall verdict (${playSafari.overall_verdict})`)
await MkvGo.playability(mkvBytes, 'not-a-real-target')
  .then(() => check(false, 'unknown playability target must reject'))
  .catch(() => check(true, 'unknown playability target rejects'))

// --- ladder: recommended ABR rungs, capped at the source resolution ---
const sourceVideo = probe.tracks.find((t) => t.type === 'video')
const rungs = await MkvGo.ladder(mkvBytes)
check(Array.isArray(rungs) && rungs.length > 0, `ladder rungs (${rungs.length})`)
check(rungs.every((r) => r.width <= sourceVideo.width && r.height <= sourceVideo.height && r.bitrate_kbps > 0),
  'ladder rungs stay within the source resolution with a positive bitrate')

// --- CENC: sample-level Common Encryption packaging (structure only; the
// decrypt round-trip itself is proven in the Go tests) ---
const cencKey = new TextEncoder().encode('0123456789abcdef') // 16 bytes
const cencKeyId = new TextEncoder().encode('KEYID-0123456789') // 16 bytes
const cencIv = new Uint8Array([0, 0, 0, 0, 0, 0, 0, 1]) // 8 bytes
const planCenc = await MkvGo.openHLS(mkvBytes,
  { segmentSeconds: 0.5, cenc: { scheme: 'cenc', key: cencKey, keyId: cencKeyId, iv: cencIv } })
const cencInit = await planCenc.resource('init.mp4')
check(cencInit.data.length > 0, 'CENC init.mp4 served')
const cencSeg = await planCenc.segment(0)
check(cencSeg.length > 0, 'CENC segment served')
planCenc.close()
await MkvGo.openHLS(mkvBytes,
  { segmentSeconds: 0.5, cenc: { scheme: 'cenc', key: new Uint8Array(4), keyId: cencKeyId, iv: cencIv } })
  .then(() => check(false, 'invalid CENC key length must reject'))
  .catch(() => check(true, 'invalid CENC key length rejects'))

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

// --- ingest: read-only serving-plan decision (playability + ladder + seek-index check) ---
const ingestPlain = await MkvGo.ingest(mkvBytes, { target: 'mse-generic' })
check(['direct-play', 'remux-hls', 'transcode'].includes(ingestPlain.strategy),
  `ingest strategy (${ingestPlain.strategy})`)
check(ingestPlain.target === 'mse-generic', `ingest target (${ingestPlain.target})`)
check(Array.isArray(ingestPlain.reasons) && ingestPlain.reasons.length > 0,
  `ingest reasons (${ingestPlain.reasons?.length})`)
check(!ingestPlain.reindexed, 'ingest never reindexes in wasm')
const ingestAnalyzed = await MkvGo.ingest(mkvBytes, { target: 'mse-generic', analyze: true })
check(ingestAnalyzed.analysis && Array.isArray(ingestAnalyzed.analysis.tracks) && ingestAnalyzed.analysis.tracks.length > 0,
  `ingest({analyze:true}) attaches analysis (${ingestAnalyzed.analysis?.tracks?.length})`)
await MkvGo.ingest(mkvBytes, { target: 'not-a-real-target' })
  .then(() => check(false, 'ingest unknown target must reject'))
  .catch(() => check(true, 'ingest unknown target rejects'))

// --- fingerprint: container-independent content identity (full read) ---
const fp = await MkvGo.fingerprint(mkvBytes)
check(/^[0-9a-f]{64}$/.test(fp.presentation), `fingerprint presentation is hex sha256 (${fp.presentation})`)
check(Array.isArray(fp.tracks) && fp.tracks.length > 0 &&
  fp.tracks.every((t) => /^[0-9a-f]{64}$/.test(t.sha256)),
  `fingerprint per-track digests (${fp.tracks?.length})`)
const fp2 = await MkvGo.fingerprint(new Uint8Array(mkvBytes))
check(fp2.presentation === fp.presentation, 'fingerprint is deterministic over the same bytes')
// Cross-container: the same encode fingerprints identically as MP4 (no local
// filesystem in WASM - the MP4 path remuxes in memory).
const fpMp4 = await MkvGo.fingerprint(mp4.data)
check(fpMp4.tracks.every((t) => fp.tracks.some((m) => m.sha256 === t.sha256)),
  'MP4 fingerprint matches the MKV digests (cross-container, in-memory)')

// --- cueHealth: head-only seek-index triage ---
const chOk = await MkvGo.cueHealth(mkvBytes)
check(chOk.total_cues >= 0 && typeof chOk.healthy === 'boolean' &&
  (chOk.healthy || (chOk.reason ?? '').length > 0),
  `cueHealth verdict on the fixture (healthy=${chOk.healthy}, cues=${chOk.total_cues})`)

// --- mapDamage: read-only damage map (salvage --dry-run twin) ---
const dmClean = await MkvGo.mapDamage(mkvBytes)
check(dmClean.clusters_copied > 0 && dmClean.bytes_skipped === 0 &&
  (dmClean.damaged_ranges ?? []).length === 0,
  `mapDamage on a clean file reports zero damage (${dmClean.clusters_copied} clusters)`)
// Splice junk right before the first cluster: the map must localize it
// exactly (the walk breaks on the junk and resyncs on the cluster), still
// writing nothing.
{
  const magic = [0x1f, 0x43, 0xb6, 0x75]
  let at = -1
  for (let i = 0; i + 4 <= mkvBytes.length; i++) {
    if (mkvBytes[i] === magic[0] && mkvBytes[i + 1] === magic[1] &&
        mkvBytes[i + 2] === magic[2] && mkvBytes[i + 3] === magic[3]) { at = i; break }
  }
  if (at > 0) {
    const junk = new Uint8Array(99).fill(0x51)
    const damaged = new Uint8Array(mkvBytes.length + junk.length)
    damaged.set(mkvBytes.subarray(0, at), 0)
    damaged.set(junk, at)
    damaged.set(mkvBytes.subarray(at), at + junk.length)
    const dm = await MkvGo.mapDamage(damaged)
    check((dm.damaged_ranges ?? []).length === 1 &&
      dm.damaged_ranges[0].start_offset === at &&
      dm.damaged_ranges[0].end_offset === at + junk.length,
      `mapDamage localizes spliced junk exactly (${JSON.stringify(dm.damaged_ranges)})`)
  } else {
    check(false, 'mapDamage splice: fixture has no cluster')
  }
}

// --- diagnose: one-call triage (index + audio delay + size coherence) ---
const diag = await MkvGo.diagnose(mkvBytes)
check(typeof diag.healthy === 'boolean' && diag.cue_health &&
  typeof diag.audio_delays_ns === 'object' &&
  (diag.healthy || (diag.findings ?? []).every((f) => f.kind && f.remedy)),
  `diagnose verdict on the fixture (healthy=${diag.healthy}, findings=${(diag.findings ?? []).length})`)
// A truncated input must yield the re-download verdict, not a parse error.
{
  const cut = mkvBytes.subarray(0, Math.floor(mkvBytes.length * 0.7))
  const dTrunc = await MkvGo.diagnose(cut)
  check(!dTrunc.healthy && (dTrunc.findings ?? []).some((f) => f.kind === 'truncated'),
    `diagnose flags a truncated input (${JSON.stringify((dTrunc.findings ?? []).map((f) => f.kind))})`)
}
// MP4 inputs route to the head-only MP4 triage - same Diagnosis shape.
{
  const dMp4 = await MkvGo.diagnose(mp4.data)
  check(dMp4.healthy === true && dMp4.cue_health === undefined,
    `diagnose(mp4) routes to the MP4 triage (healthy=${dMp4.healthy})`)
  const cut = mp4.data.subarray(0, Math.floor(mp4.data.length * 0.5))
  const dCut = await MkvGo.diagnose(cut)
  check(!dCut.healthy && (dCut.findings ?? []).some((f) => f.kind === 'truncated' || f.kind === 'no-moov'),
    `diagnose(mp4) flags a truncated input (${JSON.stringify((dCut.findings ?? []).map((f) => f.kind))})`)
}

// --- PlanHLS options: virtual audio resync + synthesized index ---
{
  const plain = await MkvGo.openHLS(mkvBytes, { segmentSeconds: 0.5 })
  // Negative = delay the audio: the fixture's audio starts at 0, so an
  // "earlier" shift would clamp to 0 and change nothing.
  const shifted = await MkvGo.openHLS(mkvBytes, { segmentSeconds: 0.5, audioShiftMs: { 2: -100 } })
  const plainInit = (await plain.resource('init_a1.mp4')).data
  const shiftedInit = (await shifted.resource('init_a1.mp4')).data
  check(Buffer.compare(plainInit, shiftedInit) !== 0, 'audioShiftMs re-bases the audio init (edit list)')
  check(Buffer.compare(await plain.segment(0), await shifted.segment(0)) === 0,
    'audioShiftMs leaves the media segments byte-identical')
  const synth = await MkvGo.openHLS(mkvBytes, { segmentSeconds: 0.5, synthesizeIndex: true })
  check(synth.numSegments === plain.numSegments, 'synthesizeIndex is a no-op on an indexed source')
  plain.close(); shifted.close(); synth.close()
}

if (failures) { console.error(`wasm smoke: ${failures} FAILURE(S)`); process.exit(1) }
console.log('wasm smoke: ALL PASS')
process.exit(0)
