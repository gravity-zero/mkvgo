//go:build js && wasm

// Command mkvgo-wasm builds mkvgo for WebAssembly (GOOS=js GOARCH=wasm). It
// registers a global `MkvGo` object whose methods mirror the library: probe,
// remuxToMP4, remuxFromMP4, remuxToWebM, remuxToHLS, openHLS, openABR,
// extractSubtitleVTT. Every method returns a Promise; inputs are Uint8Array
// (whole file in memory) or, for probe/openHLS/openABR, a Blob/File — read
// through ranged slices, so probing a 40 GB file in the browser touches only
// a few hundred kilobytes, and an on-demand plan (media segments and windowed
// subtitle renditions alike) reads only the windows a player watches.
//
// Build: make wasm (dist/wasm/mkvgo.wasm + wasm_exec.js). See docs/wasm.md.
package main

import "syscall/js"

var version = "dev"

func main() {
	api := js.Global().Get("Object").New()
	api.Set("version", js.FuncOf(func(js.Value, []js.Value) any { return version }))
	api.Set("probe", js.FuncOf(probeJS))
	api.Set("remuxToMP4", js.FuncOf(remuxToMP4JS))
	api.Set("remuxFromMP4", js.FuncOf(remuxFromMP4JS))
	api.Set("remuxToWebM", js.FuncOf(remuxToWebMJS))
	api.Set("remuxToHLS", js.FuncOf(remuxToHLSJS))
	api.Set("openHLS", js.FuncOf(openHLSJS))
	api.Set("openABR", js.FuncOf(openABRJS))
	api.Set("extractSubtitleVTT", js.FuncOf(extractSubtitleVTTJS))
	js.Global().Set("MkvGo", api)

	// Keep the Go runtime alive; work happens in the exported callbacks.
	select {}
}
