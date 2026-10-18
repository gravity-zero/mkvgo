//go:build js && wasm

// Command mkvgo-wasm builds mkvgo for WebAssembly (GOOS=js GOARCH=wasm). It
// registers a global `MkvGo` object whose methods mirror the library: probe,
// remuxToMP4, remuxFromMP4, remuxToWebM, remuxToHLS, openHLS, openABR,
// openConcat, extractSubtitleVTT, analyze, playability, ladder, ingest,
// fingerprint, mapDamage, cueHealth, trackEnds, diagnose, openForensic. Every method returns a Promise;
// inputs are Uint8Array (whole file in memory) or, for probe/openHLS/openABR/openConcat/
// analyze/playability/ladder/ingest/fingerprint/mapDamage/cueHealth/trackEnds/diagnose, a Blob/File -
// read through ranged slices, so probing a 40 GB file in the browser touches only
// a few hundred kilobytes, and an on-demand plan (media segments and windowed
// subtitle renditions alike) reads only the windows a player watches. analyze,
// fingerprint and mapDamage read the whole file (a full block-header walk / a
// full payload hash / a full tolerant walk respectively, none ever decodes);
// playability and ladder are head-only, like probe; ingest is head-only
// unless its analyze option is set; diagnose is head-mostly (it runs the
// tolerant walk only when the declared size and the real size disagree).
// The repair and patch operations (reindex,
// salvage, retime, rollback) are never performed in wasm - browser inputs are
// read-only Blobs; mapDamage is their read-only decision half (see
// docs/wasm.md).
//
// Build: make wasm (dist/wasm/mkvgo.wasm + wasm_exec.js). See docs/wasm.md.
package main

import "syscall/js"

var version = "dev"

func main() {
	api := js.Global().Get("Object").New()
	api.Set("version", jsFunc(func(js.Value, []js.Value) any { return version }))
	api.Set("probe", jsFunc(probeJS))
	api.Set("remuxToMP4", jsFunc(remuxToMP4JS))
	api.Set("remuxFromMP4", jsFunc(remuxFromMP4JS))
	api.Set("remuxToWebM", jsFunc(remuxToWebMJS))
	api.Set("remuxToHLS", jsFunc(remuxToHLSJS))
	api.Set("openHLS", jsFunc(openHLSJS))
	api.Set("openABR", jsFunc(openABRJS))
	api.Set("openConcat", jsFunc(openConcatJS))
	api.Set("openWatermark", jsFunc(openWatermarkJS))
	api.Set("openForensic", jsFunc(openForensicJS))
	api.Set("extractSubtitleVTT", jsFunc(extractSubtitleVTTJS))
	api.Set("analyze", jsFunc(analyzeJS))
	api.Set("playability", jsFunc(playabilityJS))
	api.Set("ladder", jsFunc(ladderJS))
	api.Set("ingest", jsFunc(ingestJS))
	api.Set("fingerprint", jsFunc(fingerprintJS))
	api.Set("mapDamage", jsFunc(mapDamageJS))
	api.Set("cueHealth", jsFunc(cueHealthJS))
	api.Set("trackEnds", jsFunc(trackEndsJS))
	api.Set("diagnose", jsFunc(diagnoseJS))
	js.Global().Set("MkvGo", api)

	// Keep the Go runtime alive; work happens in the exported callbacks.
	select {}
}
