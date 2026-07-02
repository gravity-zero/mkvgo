/**
 * vue.ts — copyable Vue 3 composables over the mkvgo wasm module, mirroring
 * web/react.ts. Zero dependencies beyond vue and ./mkvgo. See docs/wasm.md.
 */
import { onScopeDispose, ref, shallowRef, watch, type Ref } from 'vue'
import {
  loadMkvGo,
  type HLSPlanHandle,
  type LoadOptions,
  type MkvGoApi,
  type ProbeResult,
} from './mkvgo'

/** Load the wasm module once (shared across callers); null until ready. */
const api = shallowRef<MkvGoApi | null>(null)
let started = false

export function useMkvGo(options: LoadOptions): Ref<MkvGoApi | null> {
  if (!started) {
    started = true
    loadMkvGo(options).then((m) => (api.value = m))
  }
  return api
}

/** Probe a File head-only; re-probes when the file changes, aborts on cleanup. */
export function useProbe(file: Ref<File | null>) {
  const probe = ref<ProbeResult | null>(null)
  const error = ref<Error | null>(null)
  watch([api, file], ([mkvgo, f], _prev, onCleanup) => {
    probe.value = null
    error.value = null
    if (!mkvgo || !f) return
    const ctl = new AbortController()
    onCleanup(() => ctl.abort())
    mkvgo
      .probe(f, { signal: ctl.signal })
      .then((p) => (probe.value = p))
      .catch((e) => !ctl.signal.aborted && (error.value = e))
  }, { immediate: true })
  return { probe, error }
}

/**
 * Play a local MKV/MP4 File in a <video> through MSE: an on-demand HLS plan
 * over ranged reads of the File (bounded memory, any size), demuxed video +
 * first audio rendition fed as two SourceBuffers. Cleans up on change/unmount.
 */
export function useHLSPlayer(video: Ref<HTMLVideoElement | null>, file: Ref<File | null>) {
  const state = ref<'idle' | 'loading' | 'playing' | 'error'>('idle')
  const error = ref<Error | null>(null)

  const stop = watch([api, video, file], ([mkvgo, el, f], _prev, onCleanup) => {
    if (!mkvgo || !el || !f) return
    const ctl = new AbortController()
    let plan: HLSPlanHandle | null = null
    let url = ''
    state.value = 'loading'
    error.value = null
    onCleanup(() => {
      ctl.abort()
      plan?.close()
      if (url) URL.revokeObjectURL(url)
      el.removeAttribute('src')
    })
    ;(async () => {
      plan = await mkvgo.openHLS(f, { segmentSeconds: 6, skipUnsupported: true, signal: ctl.signal })
      const master = new TextDecoder().decode((await plan.resource('master.m3u8')).data)
      const codecs = (/CODECS="([^"]+)"/.exec(master)?.[1] ?? '').split(',')
      if (!codecs[0] || !MediaSource.isTypeSupported(`video/mp4; codecs="${codecs[0]}"`))
        throw new Error(`unsupported codecs: ${codecs.join(',')}`)
      const ms = new MediaSource()
      url = URL.createObjectURL(ms)
      el.src = url
      await new Promise<void>((r) => ms.addEventListener('sourceopen', () => r(), { once: true }))
      const feed = async (mime: string, init: string, seg: (n: number) => string) => {
        const sb = ms.addSourceBuffer(mime)
        const append = (b: BufferSource) =>
          new Promise<void>((r) => {
            sb.addEventListener('updateend', () => r(), { once: true })
            sb.appendBuffer(b)
          })
        await append((await plan!.resource(init, { signal: ctl.signal })).data)
        for (let n = 1; n <= plan!.numSegments; n++) {
          if (ctl.signal.aborted) return
          await append((await plan!.resource(seg(n), { signal: ctl.signal })).data)
        }
      }
      const pad = (n: number) => String(n).padStart(5, '0')
      const jobs = [feed(`video/mp4; codecs="${codecs[0]}"`, 'init.mp4', (n) => `seg${pad(n)}.m4s`)]
      if (codecs[1] && plan.resources.includes('init_a1.mp4'))
        jobs.push(feed(`audio/mp4; codecs="${codecs[1]}"`, 'init_a1.mp4', (n) => `seg_a1_${pad(n)}.m4s`))
      await Promise.all(jobs)
      if (!ctl.signal.aborted) {
        ms.endOfStream()
        state.value = 'playing'
      }
    })().catch((e) => {
      if (!ctl.signal.aborted) {
        error.value = e
        state.value = 'error'
      }
    })
  }, { immediate: true })

  onScopeDispose(stop)
  return { state, error }
}
