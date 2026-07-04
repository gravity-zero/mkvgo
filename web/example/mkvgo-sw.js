// mkvgo-sw.js — a Service Worker that turns the mkvgo WASM into an in-browser
// HLS origin. It intercepts requests under __mkvgo__/<id>/<resource> and answers
// them from an on-demand plan (openHLS for one file, openABR for a ladder), so a
// plain <video>/hls.js can stream a local file — even one far larger than memory
// — with zero server and zero upload. The "resource(name)" work is all Go/WASM
// (covered by wasm_smoke); this file is just the fetch router around it, and the
// serve contract it implements is exercised in Node by that smoke.

/* global importScripts, Go */

const VIRTUAL = '__mkvgo__' // path segment that marks a request for us

// parseVirtualURL splits a request pathname into {id, name} when it addresses a
// plan resource, else null. name keeps its slashes ("v2/seg00007.m4s").
function parseVirtualURL(pathname) {
  const i = pathname.indexOf('/' + VIRTUAL + '/')
  if (i < 0) return null
  const rest = pathname.slice(i + VIRTUAL.length + 2)
  const slash = rest.indexOf('/')
  if (slash < 1) return null
  return { id: rest.slice(0, slash), name: rest.slice(slash + 1) }
}

importScripts('../../dist/wasm/wasm_exec.js')

const ready = (async () => {
  const go = new Go()
  const res = await WebAssembly.instantiateStreaming(fetch('../../dist/wasm/mkvgo.wasm'), go.importObject)
  go.run(res.instance)
  while (!self.MkvGo) await new Promise((r) => setTimeout(r, 5))
})()

const plans = new Map() // id → { resource(name) } handle

self.addEventListener('install', (e) => e.waitUntil(self.skipWaiting()))
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

self.addEventListener('message', (e) => {
  const msg = e.data || {}
  if (msg.type !== 'open') return
  e.waitUntil((async () => {
    await ready
    const opts = msg.opts || { segmentSeconds: 6, skipUnsupported: true }
    const handle = msg.inputs.length > 1
      ? await self.MkvGo.openABR(msg.inputs, opts)
      : await self.MkvGo.openHLS(msg.inputs[0], opts)
    plans.set(msg.id, handle)
    e.source.postMessage({ type: 'ready', id: msg.id })
  })())
})

self.addEventListener('fetch', (e) => {
  const route = parseVirtualURL(new URL(e.request.url).pathname)
  if (!route) return // not ours: default network handling
  e.respondWith((async () => {
    await ready
    const plan = plans.get(route.id)
    if (!plan) return new Response('no such mkvgo session', { status: 404 })
    try {
      const { data, contentType, sha256 } = await plan.resource(route.name)
      // Deterministic bytes → a stable ETag straight from the content hash.
      if (e.request.headers.get('if-none-match') === `"${sha256}"`) {
        return new Response(null, { status: 304 })
      }
      return new Response(data, {
        headers: { 'Content-Type': contentType, 'ETag': `"${sha256}"`, 'Cache-Control': 'no-cache' },
      })
    } catch (err) {
      return new Response(String(err), { status: 404 })
    }
  })())
})
