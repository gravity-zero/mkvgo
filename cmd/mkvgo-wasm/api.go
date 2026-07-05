//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall/js"
	"time"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// sha256Hex is the lowercase hex SHA-256 of b — a stable content ETag for a
// resource. Because mkvgo's outputs are deterministic, the same resource always
// hashes the same, so a server/Service Worker can set ETag/If-None-Match and a
// CDN can dedup on it. Computed over bytes already in hand, so it is cheap
// relative to building the segment.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// promise runs fn on its own goroutine and returns a JS Promise for its
// result — the only sane calling convention for wasm exports, since fn may
// block (Blob reads await JS promises).
func promise(fn func() (any, error)) any {
	handler := js.FuncOf(func(this js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reject.Invoke(jsError(fmt.Sprintf("mkvgo: panic: %v", r)))
				}
			}()
			v, err := fn()
			if err != nil {
				reject.Invoke(jsError(err.Error()))
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	p := js.Global().Get("Promise").New(handler)
	handler.Release()
	return p
}

func jsError(msg string) js.Value {
	return js.Global().Get("Error").New(msg)
}

// signalContext derives a context from opts.signal (an AbortSignal), so any
// in-flight probe/remux/segment build cancels when the caller aborts —
// e.g. a React effect cleanup. Returns the context and a release function.
func signalContext(opts js.Value) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	if opts.Type() != js.TypeObject {
		return ctx, cancel
	}
	sig := opts.Get("signal")
	if sig.Type() != js.TypeObject {
		return ctx, cancel
	}
	if sig.Get("aborted").Truthy() {
		cancel()
		return ctx, cancel
	}
	var fn js.Func
	fn = js.FuncOf(func(js.Value, []js.Value) any { cancel(); return nil })
	sig.Call("addEventListener", "abort", fn)
	return ctx, func() {
		sig.Call("removeEventListener", "abort", fn)
		fn.Release()
		cancel()
	}
}

// optArg returns args[idx] or undefined.
func optArg(args []js.Value, idx int) js.Value {
	if len(args) > idx {
		return args[idx]
	}
	return js.Undefined()
}

// toGoBytes copies a Uint8Array into Go memory.
func toGoBytes(v js.Value) []byte {
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

// toUint8Array copies Go bytes out to a fresh Uint8Array.
func toUint8Array(b []byte) js.Value {
	u8 := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(u8, b)
	return u8
}

func isBlob(v js.Value) bool {
	blob := js.Global().Get("Blob")
	return blob.Type() == js.TypeFunction && v.InstanceOf(blob)
}

// sniffFormat classifies the input from its first bytes: "mkv" for an EBML
// header (Matroska/WebM), "mp4" for an ISO-BMFF box structure.
func sniffFormat(head []byte) (string, error) {
	if len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3 {
		return "mkv", nil
	}
	if len(head) >= 8 {
		switch string(head[4:8]) {
		case "ftyp", "moov", "styp", "wide", "free", "skip", "mdat":
			return "mp4", nil
		}
	}
	return "", fmt.Errorf("unrecognised container (neither an EBML/Matroska header nor an ISO-BMFF box)")
}

// probeOpts reads the optional probe options object.
type probeOpts struct {
	keyframes    bool
	bitrate      bool
	inBandColour bool
}

func readProbeOpts(args []js.Value, idx int) probeOpts {
	var o probeOpts
	if len(args) <= idx || args[idx].Type() != js.TypeObject {
		return o
	}
	v := args[idx]
	o.keyframes = v.Get("keyframes").Truthy()
	o.bitrate = v.Get("bitrate").Truthy()
	o.inBandColour = v.Get("inbandColour").Truthy()
	return o
}

// trackJSON mirrors the CLI's -json track shape: the Track fields plus the
// derived display strings the library exposes as methods.
type trackJSON struct {
	matroska.Track
	CodecLongName string  `json:"codec_long_name,omitempty"`
	ChannelLayout string  `json:"channel_layout,omitempty"`
	AvgFrameRate  float64 `json:"avg_frame_rate,omitempty"`
}

// probeResult is the JSON payload probe resolves with.
type probeResult struct {
	*matroska.Container
	Tracks        []trackJSON        `json:"tracks"`
	Format        string             `json:"format"` // "mkv" or "mp4"
	DroppedTracks []mp4.DroppedTrack `json:"dropped_tracks,omitempty"`
}

// probeJS(input: Uint8Array | Blob, opts?) → Promise<object>
func probeJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("probe: missing input") })
	}
	input := args[0]
	opts := readProbeOpts(args, 1)
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		rs, err := inputReadSeeker(input)
		if err != nil {
			return nil, err
		}
		return probeReader(ctx, rs, opts)
	})
}

// inputReadSeeker adapts the JS input to an io.ReadSeeker: a copied buffer for
// Uint8Array, a ranged reader for Blob/File (head-only work stays head-only).
func inputReadSeeker(input js.Value) (io.ReadSeeker, error) {
	if isBlob(input) {
		return newBlobReader(input), nil
	}
	if input.Type() == js.TypeObject && input.Get("byteLength").Type() == js.TypeNumber {
		return bytes.NewReader(toGoBytes(input)), nil
	}
	return nil, fmt.Errorf("input must be a Uint8Array or a Blob/File")
}

func probeReader(ctx context.Context, rs io.ReadSeeker, opts probeOpts) (any, error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(rs, head); err != nil {
		return nil, fmt.Errorf("read head: %w", err)
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	format, err := sniffFormat(head)
	if err != nil {
		return nil, err
	}

	res := probeResult{Format: format}
	switch format {
	case "mkv":
		var ro []matroska.ReadOption
		if opts.keyframes {
			ro = append(ro, matroska.WithKeyframeIndex())
		}
		if opts.bitrate {
			ro = append(ro, matroska.WithBitrate())
		}
		if opts.inBandColour {
			ro = append(ro, matroska.WithInBandColourFallback())
		}
		c, err := matroska.ReadMeta(ctx, rs, "input", ro...)
		if err != nil {
			return nil, err
		}
		res.Container = c
	case "mp4":
		c, dropped, err := mp4.ReadMeta(ctx, rs, "input",
			mp4.Options{Keyframes: opts.keyframes, InBandColour: opts.inBandColour})
		if err != nil {
			return nil, err
		}
		res.Container = c
		res.DroppedTracks = dropped
	}
	res.Tracks = make([]trackJSON, len(res.Container.Tracks))
	for i, t := range res.Container.Tracks {
		res.Tracks[i] = trackJSON{Track: t, CodecLongName: t.CodecLongName(),
			ChannelLayout: t.ChannelLayout(), AvgFrameRate: t.AvgFrameRate()}
	}
	return toJSObject(res)
}

// toJSObject marshals v to JSON and parses it on the JS side, yielding a plain
// object (numbers/strings/arrays) the caller uses directly.
func toJSObject(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return js.Global().Get("JSON").Call("parse", string(raw)), nil
}

// remuxOpts reads the optional remux options object.
func readRemuxOpts(args []js.Value, idx int) mp4.Options {
	var o mp4.Options
	if len(args) <= idx || args[idx].Type() != js.TypeObject {
		return o
	}
	v := args[idx]
	o.FastStart = v.Get("fastStart").Truthy()
	o.SkipUnsupported = v.Get("skipUnsupported").Truthy()
	o.FlattenStyledSubs = v.Get("flattenSubs").Truthy()
	o.NativeWebVTT = v.Get("nativeWebVTT").Truthy()
	o.MP3ContainerDelay = v.Get("mp3ContainerDelay").Truthy()
	o.ContentHashes = v.Get("contentHashes").Truthy()
	if s := v.Get("segmentSeconds"); s.Type() == js.TypeNumber {
		o.SegmentMs = int64(s.Float() * 1000)
	}
	if k := v.Get("keepTracks"); k.Type() == js.TypeObject && k.Get("length").Type() == js.TypeNumber {
		n := k.Get("length").Int()
		o.KeepTracks = make([]uint64, 0, n)
		for i := 0; i < n; i++ {
			o.KeepTracks = append(o.KeepTracks, uint64(k.Index(i).Int()))
		}
	}
	return o
}

// remuxResult bundles the output bytes with the dropped-track reports.
func remuxResult(data []byte, dropped []mp4.DroppedTrack) (any, error) {
	obj := js.Global().Get("Object").New()
	obj.Set("data", toUint8Array(data))
	dj, err := toJSObject(dropped)
	if err != nil {
		return nil, err
	}
	obj.Set("droppedTracks", dj)
	return obj, nil
}

// remuxToMP4JS(input: Uint8Array, opts?) → Promise<{data, droppedTracks}>
func remuxToMP4JS(_ js.Value, args []js.Value) any {
	return remuxCall(args, func(ctx context.Context, m *mkv.MemFS, o mp4.Options) (string, error) {
		return "out.mp4", mp4.RemuxToMP4(ctx, "in", "out.mp4", o)
	})
}

// remuxFromMP4JS(input: Uint8Array, opts?) → Promise<{data, droppedTracks}>
func remuxFromMP4JS(_ js.Value, args []js.Value) any {
	return remuxCall(args, func(ctx context.Context, m *mkv.MemFS, o mp4.Options) (string, error) {
		return "out.mkv", mp4.RemuxFromMP4(ctx, "in", "out.mkv", o)
	})
}

// remuxToWebMJS(input: Uint8Array) → Promise<{data, droppedTracks}>
func remuxToWebMJS(_ js.Value, args []js.Value) any {
	return remuxCall(args, func(ctx context.Context, m *mkv.MemFS, o mp4.Options) (string, error) {
		return "out.webm", matroska.RemuxToWebM(ctx, "in", "out.webm",
			matroska.Options{FS: o.FS})
	})
}

// remuxCall factors the shared shape: input bytes into a MemFS as "in", run
// the operation, return the named output plus the dropped tracks.
func remuxCall(args []js.Value, run func(context.Context, *mkv.MemFS, mp4.Options) (string, error)) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("missing input") })
	}
	input := args[0]
	opts := readRemuxOpts(args, 1)
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		if isBlob(input) {
			return nil, fmt.Errorf("remux needs a Uint8Array (the whole file in memory); Blob input is probe-only")
		}
		m := mkv.NewMemFS()
		m.Put("in", toGoBytes(input))
		var dropped []mp4.DroppedTrack
		opts.FS = m.FS()
		opts.OnDrop = func(d mp4.DroppedTrack) { dropped = append(dropped, d) }
		out, err := run(ctx, m, opts)
		if err != nil {
			return nil, err
		}
		return remuxResult(m.Get(out), dropped)
	})
}

// remuxToHLSJS(input: Uint8Array, opts?) → Promise<{files: {name: Uint8Array}, droppedTracks}>
func remuxToHLSJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("missing input") })
	}
	input := args[0]
	opts := readRemuxOpts(args, 1)
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		if isBlob(input) {
			return nil, fmt.Errorf("remuxToHLS needs a Uint8Array; Blob input is probe-only")
		}
		m := mkv.NewMemFS()
		m.Put("in", toGoBytes(input))
		var dropped []mp4.DroppedTrack
		opts.FS = m.FS()
		opts.OnDrop = func(d mp4.DroppedTrack) { dropped = append(dropped, d) }
		if err := mp4.RemuxToHLS(ctx, "in", "hls", opts); err != nil {
			return nil, err
		}
		files := js.Global().Get("Object").New()
		for _, p := range m.Paths() {
			if p == "in" {
				continue
			}
			files.Set(strings.TrimPrefix(p, "hls/"), toUint8Array(m.Get(p)))
		}
		obj := js.Global().Get("Object").New()
		obj.Set("files", files)
		dj, err := toJSObject(dropped)
		if err != nil {
			return nil, err
		}
		obj.Set("droppedTracks", dj)
		return obj, nil
	})
}

// extractSubtitleVTTJS(input: Uint8Array, trackId: number) → Promise<string>
func extractSubtitleVTTJS(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return promise(func() (any, error) { return nil, fmt.Errorf("extractSubtitleVTT: need (input, trackId)") })
	}
	input := args[0]
	trackID := uint64(args[1].Int())
	return promise(func() (any, error) {
		if isBlob(input) {
			return nil, fmt.Errorf("extractSubtitleVTT needs a Uint8Array")
		}
		data := toGoBytes(input)
		format, err := sniffFormat(data[:min(8, len(data))])
		if err != nil {
			return nil, err
		}
		m := mkv.NewMemFS()
		m.Put("in", data)
		var out strings.Builder
		switch format {
		case "mkv":
			err = matroska.ExtractSubtitleWebVTT(context.Background(), "in", trackID, &out,
				matroska.Options{FS: m.FS()})
		case "mp4":
			err = mp4.ExtractSubtitleWebVTT(context.Background(), "in", trackID, &out,
				mp4.Options{FS: m.FS()})
		}
		if err != nil {
			return nil, err
		}
		return out.String(), nil
	})
}

// blobFS adapts a Blob/File to the FS port: every Open returns an independent
// ranged reader over the same Blob, so PlanHLS's bounded reads and concurrent
// Segment calls never load the file into memory.
func blobFS(blob js.Value) *mkv.FS {
	size := int64(blob.Get("size").Float())
	return &mkv.FS{
		Open: func(string) (mkv.ReadSeekCloser, error) { return newBlobReader(blob), nil },
		Stat: func(p string) (os.FileInfo, error) { return blobInfo{name: p, size: size}, nil },
	}
}

type blobInfo struct {
	name string
	size int64
}

func (i blobInfo) Name() string       { return i.name }
func (i blobInfo) Size() int64        { return i.size }
func (i blobInfo) Mode() os.FileMode  { return 0o444 }
func (i blobInfo) ModTime() time.Time { return time.Time{} }
func (i blobInfo) IsDir() bool        { return false }
func (i blobInfo) Sys() any           { return nil }

// openHLSJS(input: Uint8Array | Blob, opts?) → Promise<handle>. The handle
// serves an HLS presentation on demand: `resources` lists every name a player
// requests, `resource(name)` builds it (data + contentType), `segment(n)` is
// the 0-based media-segment shortcut, `close()` releases the callbacks. A
// Blob/File input is read through ranged slices — playing a file far larger
// than memory stays memory-bounded, only the watched windows are ever read.
func openHLSJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("openHLS: missing input") })
	}
	input := args[0]
	opts := readRemuxOpts(args, 1)
	openCtx, openRelease := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer openRelease()
		if isBlob(input) {
			opts.FS = blobFS(input)
		} else if input.Type() == js.TypeObject && input.Get("byteLength").Type() == js.TypeNumber {
			m := mkv.NewMemFS()
			m.Put("in", toGoBytes(input))
			opts.FS = m.FS()
		} else {
			return nil, fmt.Errorf("openHLS: input must be a Uint8Array or a Blob/File")
		}
		plan, err := mp4.PlanHLS(openCtx, "in", opts)
		if err != nil {
			return nil, err
		}

		h := js.Global().Get("Object").New()
		h.Set("numSegments", plan.NumSegments())
		names := plan.Resources()
		arr := js.Global().Get("Array").New(len(names))
		for i, n := range names {
			arr.SetIndex(i, n)
		}
		h.Set("resources", arr)

		resourceFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("resource: missing name") })
			}
			name := rargs[0].String()
			ctx, release := signalContext(optArg(rargs, 1))
			return promise(func() (any, error) {
				defer release()
				data, mime, err := plan.Resource(ctx, name)
				if err != nil {
					return nil, err
				}
				obj := js.Global().Get("Object").New()
				obj.Set("data", toUint8Array(data))
				obj.Set("contentType", mime)
				obj.Set("sha256", sha256Hex(data))
				return obj, nil
			})
		})
		segmentFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("segment: missing index") })
			}
			n := rargs[0].Int()
			ctx, release := signalContext(optArg(rargs, 1))
			return promise(func() (any, error) {
				defer release()
				data, err := plan.Segment(ctx, n)
				if err != nil {
					return nil, err
				}
				return toUint8Array(data), nil
			})
		})
		var closeFn js.Func
		closeFn = js.FuncOf(func(js.Value, []js.Value) any {
			resourceFn.Release()
			segmentFn.Release()
			closeFn.Release()
			return nil
		})
		h.Set("resource", resourceFn)
		h.Set("segment", segmentFn)
		h.Set("close", closeFn)
		return h, nil
	})
}

// isUint8 reports whether v is a typed array / ArrayBuffer-like (has byteLength).
func isUint8(v js.Value) bool {
	return v.Type() == js.TypeObject && v.Get("byteLength").Type() == js.TypeNumber
}

// multiSourceFS maps each input (Uint8Array or Blob/File) to a distinct source
// path "src{i}" so PlanABR reads the right variant per path: Uint8Arrays live in
// a MemFS, Blobs are served through independent ranged readers (memory-bounded).
func multiSourceFS(inputs []js.Value) (*mkv.FS, []string, error) {
	paths := make([]string, len(inputs))
	blobs := map[string]js.Value{}
	mem := mkv.NewMemFS()
	for i, in := range inputs {
		p := fmt.Sprintf("src%d", i)
		paths[i] = p
		switch {
		case isBlob(in):
			blobs[p] = in
		case isUint8(in):
			mem.Put(p, toGoBytes(in))
		default:
			return nil, nil, fmt.Errorf("openABR: input %d must be a Uint8Array or a Blob/File", i)
		}
	}
	memFS := mem.FS()
	if len(blobs) == 0 {
		return memFS, paths, nil
	}
	fs := &mkv.FS{
		Open: func(p string) (mkv.ReadSeekCloser, error) {
			if b, ok := blobs[p]; ok {
				return newBlobReader(b), nil
			}
			return memFS.Open(p)
		},
		Stat: func(p string) (os.FileInfo, error) {
			if b, ok := blobs[p]; ok {
				return blobInfo{name: p, size: int64(b.Get("size").Float())}, nil
			}
			return memFS.Stat(p)
		},
	}
	return fs, paths, nil
}

// openABRJS(inputs: Array<Uint8Array | Blob>, opts?) → Promise<handle>. inputs
// are the pre-encoded quality variants of one title, best first. The handle
// serves the whole multi-variant presentation on demand: `numVariants`,
// `resources` (every name a player requests — "master.m3u8", "v1/init.mp4",
// "v2/seg00007.m4s", …), `resource(name)` builds it (data + contentType),
// `close()` releases the callbacks. Blob variants are read through ranged
// slices, so a client-side ABR ladder of huge local files stays memory-bounded.
func openABRJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("openABR: missing inputs") })
	}
	inputsVal := args[0]
	if inputsVal.Type() != js.TypeObject || inputsVal.Get("length").Type() != js.TypeNumber {
		return promise(func() (any, error) {
			return nil, fmt.Errorf("openABR: first argument must be an array of Uint8Array|Blob (best quality first)")
		})
	}
	n := inputsVal.Get("length").Int()
	inputs := make([]js.Value, n)
	for i := 0; i < n; i++ {
		inputs[i] = inputsVal.Index(i)
	}
	opts := readRemuxOpts(args, 1)
	openCtx, openRelease := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer openRelease()
		fs, paths, err := multiSourceFS(inputs)
		if err != nil {
			return nil, err
		}
		opts.FS = fs
		plan, err := mp4.PlanABR(openCtx, paths, opts)
		if err != nil {
			return nil, err
		}

		h := js.Global().Get("Object").New()
		h.Set("numVariants", plan.NumVariants())
		names := plan.Resources()
		arr := js.Global().Get("Array").New(len(names))
		for i, nm := range names {
			arr.SetIndex(i, nm)
		}
		h.Set("resources", arr)

		resourceFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("resource: missing name") })
			}
			name := rargs[0].String()
			ctx, release := signalContext(optArg(rargs, 1))
			return promise(func() (any, error) {
				defer release()
				data, mime, err := plan.Resource(ctx, name)
				if err != nil {
					return nil, err
				}
				obj := js.Global().Get("Object").New()
				obj.Set("data", toUint8Array(data))
				obj.Set("contentType", mime)
				obj.Set("sha256", sha256Hex(data))
				return obj, nil
			})
		})
		var closeFn js.Func
		closeFn = js.FuncOf(func(js.Value, []js.Value) any {
			resourceFn.Release()
			closeFn.Release()
			return nil
		})
		h.Set("resource", resourceFn)
		h.Set("close", closeFn)
		return h, nil
	})
}
