/**
 * react.ts - copyable React hooks over the mkvgo wasm module. Zero
 * dependencies beyond react and ./mkvgo. See docs/wasm.md.
 */
import { useEffect, useRef, useState } from 'react'
import {
  loadMkvGo,
  type LoadOptions,
  type MkvGoApi,
  type ProbeResult,
  type HLSPlanHandle,
} from './mkvgo'

/** Load the wasm module once; null until ready. */
export function useMkvGo(options: LoadOptions): MkvGoApi | null {
  const [api, setApi] = useState<MkvGoApi | null>(null)
  const opts = useRef(options)
  useEffect(() => {
    let live = true
    loadMkvGo(opts.current).then((m) => live && setApi(m))
    return () => {
      live = false
    }
  }, [])
  return api
}

/** Probe a File head-only (no size limit); aborts if the file changes or unmounts. */
export function useProbe(mkvgo: MkvGoApi | null, file: File | null) {
  const [probe, setProbe] = useState<ProbeResult | null>(null)
  const [error, setError] = useState<Error | null>(null)
  useEffect(() => {
    setProbe(null)
    setError(null)
    if (!mkvgo || !file) return
    const ctl = new AbortController()
    mkvgo
      .probe(file, { signal: ctl.signal })
      .then(setProbe)
      .catch((e) => !ctl.signal.aborted && setError(e))
    return () => ctl.abort()
  }, [mkvgo, file])
  return { probe, error }
}

/**
 * Play a local MKV File in a <video> through MSE: an on-demand HLS plan over
 * ranged reads of the File (bounded memory, any size), demuxed video + first
 * audio rendition fed as two SourceBuffers. Cleans up on unmount/change.
 */
export function useHLSPlayer(
  mkvgo: MkvGoApi | null,
  video: React.RefObject<HTMLVideoElement | null>,
  file: File | null,
) {
  const [state, setState] = useState<'idle' | 'loading' | 'playing' | 'error'>('idle')
  const [error, setError] = useState<Error | null>(null)
  useEffect(() => {
    if (!mkvgo || !file || !video.current) return
    const el = video.current
    const ctl = new AbortController()
    let plan: HLSPlanHandle | null = null
    let url = ''
    setState('loading')
    setError(null)
    ;(async () => {
      plan = await mkvgo.openHLS(file, { segmentSeconds: 6, skipUnsupported: true, signal: ctl.signal })
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
        setState('playing')
      }
    })().catch((e) => {
      if (!ctl.signal.aborted) {
        setError(e)
        setState('error')
      }
    })
    return () => {
      ctl.abort()
      plan?.close()
      if (url) URL.revokeObjectURL(url)
      el.removeAttribute('src')
    }
  }, [mkvgo, file, video])
  return { state, error }
}
