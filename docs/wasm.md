# mkvgo in the browser (WebAssembly)

mkvgo compiles to WebAssembly — the whole probe/remux/HLS engine runs
client-side, in browsers, web workers and Node. Zero dependencies carries over:
the artifact is **~4.7 MB raw, ~1.3 MB gzipped** (ffmpeg.wasm is ~30 MB), and
nothing ever leaves the user's machine.

What it enables:

- **Probe any file instantly, whatever its size.** A `File`/`Blob` is read
  through ranged slices, and mkvgo's probe is head-only — inspecting a 40 GB
  MKV in a `<input type=file>` transfers a few hundred kilobytes and takes
  milliseconds. No upload, no server.
- **Remux MKV → MP4 (and back), package HLS, extract subtitles** — for files
  that fit in memory — without transcoding: the original frames are copied
  into the new container.
- **Play an MKV in a `<video>` tag** with no server and no transcoding:
  `remuxToHLS` emits fragmented MP4, which Media Source Extensions accept
  directly (see the [runnable demo](../web/example/index.html)).

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
is the typed wrapper around it (copy the file into your project — it has no
dependencies). Every method returns a Promise and every error is a rejection.

| Method | Input | Result |
|---|---|---|
| `probe(input, opts?)` | `Uint8Array` **or `Blob`/`File`** (head-only, any size) | metadata object — same shape as the CLI `-json` output, plus `format: "mkv"\|"mp4"` |
| `remuxToMP4(input, opts?)` | `Uint8Array` (MKV/WebM) | `{ data: Uint8Array, droppedTracks }` |
| `remuxFromMP4(input, opts?)` | `Uint8Array` (MP4/MOV) | `{ data, droppedTracks }` (MKV) |
| `remuxToWebM(input)` | `Uint8Array` (MKV) | `{ data, droppedTracks }` (VP8/VP9/AV1 + Opus/Vorbis only) |
| `remuxToHLS(input, opts?)` | `Uint8Array` (MKV/WebM) | `{ files: {name → Uint8Array}, droppedTracks }` — `master.m3u8`, `playlist.m3u8`, `init.mp4`, `seg*.m4s`, subtitle renditions |
| `extractSubtitleVTT(input, trackId)` | `Uint8Array` (MKV or MP4) | WebVTT `string` |
| `version()` | — | version `string` |

Probe options: `{ keyframes?, bitrate?, inbandColour? }`. Remux options:
`{ fastStart?, skipUnsupported?, flattenSubs?, nativeWebVTT?,
mp3ContainerDelay?, contentHashes?, segmentSeconds? }` — the same semantics as
the CLI flags ([cli.md](cli.md)).

Input format is sniffed from the first bytes (EBML magic vs ISO-BMFF box), not
from a file name.

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

## React

A hook owning the module and a probe component:

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
              #{t.id} {t.type} — {t.codec_long_name ?? t.codec}
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

A composable and a component:

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

```vue
<!-- MediaInspector.vue -->
<script setup lang="ts">
import { ref } from 'vue'
import { useMkvGo } from './useMkvGo'
import type { ProbeResult } from './mkvgo'

const mkvgo = useMkvGo()
const probe = ref<ProbeResult | null>(null)

async function onFile(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (file && mkvgo.value) probe.value = await mkvgo.value.probe(file)
}
</script>

<template>
  <input type="file" accept=".mkv,.webm,.mp4,.mov" :disabled="!mkvgo" @change="onFile" />
  <ul v-if="probe">
    <li v-for="t in probe.tracks" :key="t.id">
      #{{ t.id }} {{ t.type }} — {{ t.codec_long_name ?? t.codec }}
      <template v-if="t.width"> {{ t.width }}×{{ t.height }}</template>
    </li>
  </ul>
</template>
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

- **Remux needs the whole file in memory** (input + output simultaneously —
  wasm32 addresses 4 GB; in practice keep inputs under ~1.5 GB). `probe`
  accepts a `Blob`/`File` and has **no size limit** — it reads head-only.
- **No transcoding**, by design: only the codecs the native remuxers support
  are carried (see [cli.md](cli.md)); unsupported tracks fail or are dropped
  with `skipUnsupported`/reported in `droppedTracks`.
- One in-flight module instance; calls are safe to issue concurrently (each
  runs on its own goroutine), but they share one wasm heap.
