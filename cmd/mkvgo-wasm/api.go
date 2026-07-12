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
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mp4"
)

// sha256Hex is the lowercase hex SHA-256 of b - a stable content ETag for a
// resource. Because mkvgo's outputs are deterministic, the same resource always
// hashes the same, so a server/Service Worker can set ETag/If-None-Match and a
// CDN can dedup on it. Computed over bytes already in hand, so it is cheap
// relative to building the segment.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// promise runs fn on its own goroutine and returns a JS Promise for its
// result - the only sane calling convention for wasm exports, since fn may
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
// in-flight probe/remux/segment build cancels when the caller aborts  -
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
// derived display strings the library exposes as methods - codec/channel
// names, aspect ratios, colour names and the one-word HDR classification.
type trackJSON struct {
	matroska.Track
	CodecLongName       string  `json:"codec_long_name,omitempty"`
	ChannelLayout       string  `json:"channel_layout,omitempty"`
	AvgFrameRate        float64 `json:"avg_frame_rate,omitempty"`
	SampleAspectRatio   string  `json:"sample_aspect_ratio,omitempty"`
	DisplayAspectRatio  string  `json:"display_aspect_ratio,omitempty"`
	ColorSpaceName      string  `json:"color_space_name,omitempty"`
	ColorTransferName   string  `json:"color_transfer_name,omitempty"`
	ColorPrimariesName  string  `json:"color_primaries_name,omitempty"`
	ColorRangeName      string  `json:"color_range_name,omitempty"`
	HDRFormat           string  `json:"hdr_format,omitempty"`
	StereoModeName      string  `json:"stereo_mode_name,omitempty"`
	ResolvedLanguage    string  `json:"resolved_language,omitempty"`
	EffectiveSampleRate float64 `json:"effective_sample_rate,omitempty"`
}

// trackJSONOf fills every derived field from the Track's methods.
func trackJSONOf(t matroska.Track) trackJSON {
	return trackJSON{Track: t,
		CodecLongName:       t.CodecLongName(),
		ChannelLayout:       t.ChannelLayout(),
		AvgFrameRate:        t.AvgFrameRate(),
		SampleAspectRatio:   t.SampleAspectRatio(),
		DisplayAspectRatio:  t.DisplayAspectRatio(),
		ColorSpaceName:      t.ColorSpaceName(),
		ColorTransferName:   t.ColorTransferName(),
		ColorPrimariesName:  t.ColorPrimariesName(),
		ColorRangeName:      t.ColorRangeName(),
		HDRFormat:           t.HDRFormat(),
		StereoModeName:      t.StereoModeName(),
		ResolvedLanguage:    t.ResolvedLanguage(),
		EffectiveSampleRate: t.EffectiveSampleRate(),
	}
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
		res.Tracks[i] = trackJSONOf(t)
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
	if s := v.Get("subOffsetMs"); s.Type() == js.TypeNumber {
		o.SubtitleOffsetMs = int64(s.Float())
	}
	o.SynthesizeIndex = v.Get("synthesizeIndex").Truthy()
	// audioShiftMs: {trackNumber: ms} - the browser-side ms shape converts to
	// the library's nanoseconds here.
	if m := v.Get("audioShiftMs"); m.Type() == js.TypeObject {
		keys := js.Global().Get("Object").Call("keys", m)
		for i, n := 0, keys.Get("length").Int(); i < n; i++ {
			k := keys.Index(i).String()
			track, err := strconv.ParseUint(k, 10, 64)
			if err != nil {
				continue
			}
			if val := m.Get(k); val.Type() == js.TypeNumber {
				if o.AudioPresentationShift == nil {
					o.AudioPresentationShift = map[uint64]int64{}
				}
				o.AudioPresentationShift[track] = int64(val.Float()) * 1_000_000
			}
		}
	}
	return o
}

// readKeepLangs reads the optional `keepLangs` array of language codes from
// the given opts object (openConcat's CLI-equivalent --keep-lang).
func readKeepLangs(v js.Value) []string {
	if v.Type() != js.TypeObject {
		return nil
	}
	k := v.Get("keepLangs")
	if k.Type() != js.TypeObject || k.Get("length").Type() != js.TypeNumber {
		return nil
	}
	n := k.Get("length").Int()
	langs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if l := k.Index(i).String(); l != "" {
			langs = append(langs, strings.ToLower(l))
		}
	}
	return langs
}

// resolveKeepLangs turns language codes into a KeepTracks ID list, resolved
// from the first source's track metadata: every video track (HLS needs
// video) plus every audio/subtitle track whose language matches - the wasm
// counterpart of the CLI's resolveKeepLangs (cmd/mkvgo/commands/hls.go).
// Concat requires every part's kept track layout to align, so the IDs
// resolved from part 0 apply uniformly to every source.
func resolveKeepLangs(ctx context.Context, fs *mkv.FS, path string, langs []string) ([]uint64, error) {
	rs, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
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
	var tracks []mkv.Track
	switch format {
	case "mkv":
		c, err := matroska.ReadMeta(ctx, rs, path)
		if err != nil {
			return nil, err
		}
		tracks = c.Tracks
	case "mp4":
		c, _, err := mp4.ReadMeta(ctx, rs, path)
		if err != nil {
			return nil, err
		}
		tracks = c.Tracks
	}
	want := make(map[string]bool, len(langs))
	for _, l := range langs {
		want[l] = true
	}
	var ids []uint64
	for _, t := range tracks {
		if t.Type == mkv.VideoTrack || want[strings.ToLower(t.Language)] {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

// readCENCOpts reads an optional `cenc` object from the given opts object:
// { scheme: "cenc"|"cbcs", key: Uint8Array(16), keyId: Uint8Array(16),
// iv: Uint8Array(8|16), keyURI?: string }. Length/scheme validation happens
// on the Go side (mp4.CENCOptions.validate, via PlanHLS/PlanABR), so a bad
// shape surfaces as the same rejected-promise error every other option does.
// PSSH boxes are not exposed in this wasm build (see docs/wasm.md).
func readCENCOpts(v js.Value) (*mp4.CENCOptions, error) {
	if v.Type() != js.TypeObject {
		return nil, nil
	}
	c := v.Get("cenc")
	if c.Type() != js.TypeObject {
		return nil, nil
	}
	key, keyID, iv := c.Get("key"), c.Get("keyId"), c.Get("iv")
	if !isUint8(key) || !isUint8(keyID) || !isUint8(iv) {
		return nil, fmt.Errorf("cenc: key, keyId and iv must be Uint8Array")
	}
	out := &mp4.CENCOptions{Key: toGoBytes(key), KeyID: toGoBytes(keyID), IV: toGoBytes(iv)}
	if s := c.Get("scheme"); s.Type() == js.TypeString {
		out.Scheme = s.String()
	}
	if u := c.Get("keyURI"); u.Type() == js.TypeString {
		out.KeyURI = u.String()
	}
	return out, nil
}

// readEncryptOpts reads an optional `encrypt` object for AES-128 whole-segment
// HLS encryption (RFC 8216), the counterpart of the CLI --aes-key/--aes-key-uri
// flags and mp4.Options.Encrypt: { key: Uint8Array(16), keyURI?: string,
// iv?: Uint8Array(16) }. Length validation happens on the Go side
// (mp4.HLSEncryption.validate, via PlanHLS/PlanABR), so a bad shape surfaces as
// the same rejected-promise error every other option does. Leaving iv unset
// uses the spec default (the per-segment media sequence number).
func readEncryptOpts(v js.Value) (*mp4.HLSEncryption, error) {
	if v.Type() != js.TypeObject {
		return nil, nil
	}
	e := v.Get("encrypt")
	if e.Type() != js.TypeObject {
		return nil, nil
	}
	// Rotating schedule: { rotateEverySegments: N, keys: [{ key, keyURI, iv? }] }.
	// A leaked key then decrypts only its period. Length/count validation happens
	// on the Go side (HLSEncryption.validate, via PlanHLS/PlanABR).
	if rot := e.Get("rotateEverySegments"); rot.Type() == js.TypeNumber {
		out := &mp4.HLSEncryption{RotateEverySegments: rot.Int()}
		keys := e.Get("keys")
		if keys.Type() != js.TypeObject || keys.Get("length").Type() != js.TypeNumber {
			return nil, fmt.Errorf("encrypt: rotateEverySegments needs a keys array")
		}
		for i := 0; i < keys.Get("length").Int(); i++ {
			kv := keys.Index(i)
			k, err := readHLSKey(kv, fmt.Sprintf("encrypt.keys[%d]", i))
			if err != nil {
				return nil, err
			}
			out.Keys = append(out.Keys, k)
		}
		return out, nil
	}

	k, err := readHLSKey(e, "encrypt")
	if err != nil {
		return nil, err
	}
	return &mp4.HLSEncryption{Key: k.Key, KeyURI: k.KeyURI, IV: k.IV}, nil
}

// readHLSKey reads { key: Uint8Array, keyURI?: string, iv?: Uint8Array }.
func readHLSKey(v js.Value, what string) (mp4.HLSKey, error) {
	key := v.Get("key")
	if !isUint8(key) {
		return mp4.HLSKey{}, fmt.Errorf("%s: key must be a Uint8Array", what)
	}
	out := mp4.HLSKey{Key: toGoBytes(key)}
	if u := v.Get("keyURI"); u.Type() == js.TypeString {
		out.KeyURI = u.String()
	}
	if iv := v.Get("iv"); iv.Type() != js.TypeUndefined && iv.Type() != js.TypeNull {
		if !isUint8(iv) {
			return mp4.HLSKey{}, fmt.Errorf("%s: iv must be a Uint8Array", what)
		}
		out.IV = toGoBytes(iv)
	}
	return out, nil
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
// Blob/File input is read through ranged slices - playing a file far larger
// than memory stays memory-bounded, only the watched windows are ever read.
func openHLSJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("openHLS: missing input") })
	}
	input := args[0]
	opts := readRemuxOpts(args, 1)
	cenc, cencErr := readCENCOpts(optArg(args, 1))
	enc, encErr := readEncryptOpts(optArg(args, 1))
	openCtx, openRelease := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer openRelease()
		if cencErr != nil {
			return nil, cencErr
		}
		if encErr != nil {
			return nil, encErr
		}
		opts.CENC = cenc
		opts.Encrypt = enc
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

// singleSourceFS adapts one JS input (Uint8Array or Blob/File) to an mkv.FS
// serving it at path "in": a MemFS for a Uint8Array (already in memory), a
// ranged blobFS for a Blob/File (memory-bounded). Shared by analyze (full
// block-header walk) and playability/ladder (head-only), same input handling
// as the other single-source bindings.
func singleSourceFS(input js.Value) (*mkv.FS, error) {
	if isBlob(input) {
		return blobFS(input), nil
	}
	if isUint8(input) {
		m := mkv.NewMemFS()
		m.Put("in", toGoBytes(input))
		return m.FS(), nil
	}
	return nil, fmt.Errorf("input must be a Uint8Array or a Blob/File")
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
// `resources` (every name a player requests - "master.m3u8", "v1/init.mp4",
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
	cenc, cencErr := readCENCOpts(optArg(args, 1))
	enc, encErr := readEncryptOpts(optArg(args, 1))
	openCtx, openRelease := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer openRelease()
		if cencErr != nil {
			return nil, cencErr
		}
		if encErr != nil {
			return nil, encErr
		}
		opts.CENC = cenc
		opts.Encrypt = enc
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

// openConcatJS(inputs: Array<Uint8Array | Blob>, opts?) → Promise<handle>.
// inputs are the sources to play as ONE continuous session, in playback
// order (see mp4.PlanConcat). The handle serves the whole concatenated
// presentation on demand: `numParts`, `resources` (every name a player
// requests - "master.m3u8", "playlist.m3u8", "p0/init.mp4", "p1/seg00007.m4s",
// …), `resource(name)` builds it (data + contentType), `close()` releases the
// callbacks. Blob sources are read through ranged slices, so a concatenated
// session over huge local files stays memory-bounded. `opts.keepLangs`
// resolves a language-based track subset (video always kept) from the first
// source's metadata, mirroring the CLI's --keep-lang.
func openConcatJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("openConcat: missing inputs") })
	}
	inputsVal := args[0]
	if inputsVal.Type() != js.TypeObject || inputsVal.Get("length").Type() != js.TypeNumber {
		return promise(func() (any, error) {
			return nil, fmt.Errorf("openConcat: first argument must be an array of Uint8Array|Blob (playback order)")
		})
	}
	n := inputsVal.Get("length").Int()
	inputs := make([]js.Value, n)
	for i := 0; i < n; i++ {
		inputs[i] = inputsVal.Index(i)
	}
	opts := readRemuxOpts(args, 1)
	keepLangs := readKeepLangs(optArg(args, 1))
	openCtx, openRelease := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer openRelease()
		fs, paths, err := multiSourceFS(inputs)
		if err != nil {
			return nil, err
		}
		if len(opts.KeepTracks) == 0 && len(keepLangs) > 0 {
			ids, err := resolveKeepLangs(openCtx, fs, paths[0], keepLangs)
			if err != nil {
				return nil, err
			}
			opts.KeepTracks = ids
		}
		opts.FS = fs
		plan, err := mp4.PlanConcat(openCtx, paths, opts)
		if err != nil {
			return nil, err
		}

		h := js.Global().Get("Object").New()
		h.Set("numParts", plan.NumParts())
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

// analyzeJS(input: Uint8Array | Blob, opts?) → Promise<object (AnalyzeReport)>.
// Unlike probe, this needs a FULL block-header walk (frame/keyframe counts,
// GOP spans, bitrate), never just the head - so the input handling matches
// remux/openHLS: a Uint8Array is read in place, a Blob/File through ranged
// slices, so a large file stays memory-bounded even though every block
// header is visited.
func analyzeJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("analyze: missing input") })
	}
	input := args[0]
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("analyze: %w", err)
		}
		report, err := ops.Analyze(ctx, "in", mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(report)
	})
}

// playabilityJS(input: Uint8Array | Blob, target?: string, opts?) →
// Promise<object (PlayabilityReport)>. target defaults to "mse-generic";
// an unrecognised name rejects. Head-only, like probe.
func playabilityJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("playability: missing input") })
	}
	input := args[0]
	targetName := "mse-generic"
	if len(args) > 1 && args[1].Type() == js.TypeString {
		targetName = args[1].String()
	}
	ctx, release := signalContext(optArg(args, 2))
	return promise(func() (any, error) {
		defer release()
		target, ok := ops.TargetByName(targetName)
		if !ok {
			return nil, fmt.Errorf("playability: unknown target %q", targetName)
		}
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("playability: %w", err)
		}
		report, err := ops.Playability(ctx, "in", target, mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(report)
	})
}

// ladderJS(input: Uint8Array | Blob, opts?) → Promise<Array (Rung[])>.
// Head-only, like probe.
func ladderJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("ladder: missing input") })
	}
	input := args[0]
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("ladder: %w", err)
		}
		rungs, err := ops.RecommendLadderFor(ctx, "in", mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(rungs)
	})
}

// ingestOpts reads the optional ingest options object: { target?, analyze? }.
func readIngestOpts(args []js.Value, idx int) (target string, analyze bool) {
	target = "mse-generic"
	if len(args) <= idx || args[idx].Type() != js.TypeObject {
		return target, analyze
	}
	v := args[idx]
	if t := v.Get("target"); t.Type() == js.TypeString {
		target = t.String()
	}
	analyze = v.Get("analyze").Truthy()
	return target, analyze
}

// ingestJS(input: Uint8Array | Blob, opts?) → Promise<object (ServingPlan)>.
// Read-only decision client: Reindex is always false here - a browser MemFS
// write is not the use case a wasm caller has (there is nothing durable to
// write back to). A server/CLI that needs the repairing path calls
// ops.Ingest(Reindex: true) directly (or the CLI's own ingest command); this
// binding only ever reports NeedsReindex/ReindexInPlacePossible for the
// caller to act on out of band. opts.analyze also runs the full
// block-header walk (ops.Analyze) and attaches it to the plan, regardless of
// the decided strategy - see IngestOptions.IncludeAnalysis. Input handling
// is head-only-capable but works the same whether or not analyze forces a
// full walk: singleSourceFS's ranged Blob reader serves either.
func ingestJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("ingest: missing input") })
	}
	input := args[0]
	target, analyze := readIngestOpts(args, 1)
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("ingest: %w", err)
		}
		plan, err := ops.Ingest(ctx, "in", ops.IngestOptions{
			Options:         mkv.Options{FS: fs},
			Target:          target,
			IncludeAnalysis: analyze,
			Reindex:         false,
		})
		if err != nil {
			return nil, err
		}
		return toJSObject(plan)
	})
}

// fingerprintJS(input: Uint8Array | Blob, opts?) → Promise<object (FingerprintReport)>.
// Container-independent content identity: a per-track SHA-256 over frame
// payloads in decode order, plus a Presentation hash a media library can use
// to dedup re-muxes of identical content regardless of container metadata or
// track order. This is a FULL read - every frame payload is read and hashed -
// so the input handling matches analyze: a Uint8Array is read in place, a
// Blob/File through ranged slices, staying memory-bounded even though every
// payload byte is visited. Matroska/WebM only (see ops.Fingerprint).
func fingerprintJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("fingerprint: missing input") })
	}
	input := args[0]
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("fingerprint: %w", err)
		}
		report, err := ops.Fingerprint(ctx, "in", mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(report)
	})
}

// cueHealthJS(input, opts?) -> Promise<CueHealthReport>: head-only triage of
// the seek index - which tracks the CuePoints reference - in milliseconds
// even on a Blob (SeekHead-guided ranged reads, no cluster walk). It spots
// the dormant defect where a file's index exists yet keys on the wrong
// tracks, so every seek lands mid-GOP while every "indexed?" check passes.
func cueHealthJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("cueHealth: missing input") })
	}
	input := args[0]
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("cueHealth: %w", err)
		}
		report, err := ops.CueHealth(ctx, "in", mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(report)
	})
}

// diagnoseJS(input, opts?) -> Promise<Diagnosis>: one-call triage - seek-index
// health, per-track audio start delays, declared-size coherence, and (only
// when the size check suggests damage) the full tolerant walk - each finding
// carrying its remedy (reindex / retime / resync / re-download). Head-mostly:
// on a healthy file it costs the head plus the first cluster(s).
func diagnoseJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("diagnose: missing input") })
	}
	input := args[0]
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("diagnose: %w", err)
		}
		d, err := ops.Diagnose(ctx, "in", mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(d)
	})
}

// openForensicJS(input, opts?) -> Promise<ForensicHandle>: single-source
// forensic A/B session watermarking. Where openWatermark needs TWO
// pre-encoded variants, this derives variant B from ONE source by dropping a
// disposable H.264 frame per segment (timing-compensated: the manifest and
// durations are shared). The handle mirrors openWatermark's - numSegments,
// masterPlaylist, mediaPlaylist, init, segment(n, fromB), segmentForPattern -
// plus distinct(n): whether segment n carries a watermark bit at all.
func openForensicJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("openForensic: missing input") })
	}
	input := args[0]
	opts := readRemuxOpts(args, 1)
	openCtx, openRelease := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer openRelease()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("openForensic: %w", err)
		}
		opts.FS = fs
		fp, err := mp4.PlanForensic(openCtx, "in", opts)
		if err != nil {
			return nil, err
		}

		h := js.Global().Get("Object").New()
		h.Set("numSegments", fp.NumSegments())
		h.Set("masterPlaylist", toUint8Array(fp.MasterPlaylist()))
		h.Set("mediaPlaylist", toUint8Array(fp.MediaPlaylist()))
		h.Set("init", toUint8Array(fp.InitSegment()))

		segmentFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("segment: missing index") })
			}
			n := rargs[0].Int()
			fromB := len(rargs) > 1 && rargs[1].Truthy()
			ctx, release := signalContext(optArg(rargs, 2))
			return promise(func() (any, error) {
				defer release()
				data, err := fp.Segment(ctx, n, fromB)
				if err != nil {
					return nil, err
				}
				return toUint8Array(data), nil
			})
		})
		patternFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 2 || !isUint8(rargs[1]) {
				return promise(func() (any, error) {
					return nil, fmt.Errorf("segmentForPattern: needs an index and a Uint8Array pattern")
				})
			}
			n := rargs[0].Int()
			pattern := toGoBytes(rargs[1])
			ctx, release := signalContext(optArg(rargs, 2))
			return promise(func() (any, error) {
				defer release()
				data, err := fp.SegmentForPattern(ctx, n, pattern)
				if err != nil {
					return nil, err
				}
				return toUint8Array(data), nil
			})
		})
		distinctFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("distinct: missing index") })
			}
			n := rargs[0].Int()
			ctx, release := signalContext(optArg(rargs, 1))
			return promise(func() (any, error) {
				defer release()
				d, err := fp.Distinct(ctx, n)
				if err != nil {
					return nil, err
				}
				return d, nil
			})
		})
		var closeFn js.Func
		closeFn = js.FuncOf(func(js.Value, []js.Value) any {
			segmentFn.Release()
			patternFn.Release()
			distinctFn.Release()
			closeFn.Release()
			return nil
		})
		h.Set("segment", segmentFn)
		h.Set("segmentForPattern", patternFn)
		h.Set("distinct", distinctFn)
		h.Set("close", closeFn)
		return h, nil
	})
}

// mapDamageJS(input, opts?) -> Promise<SalvageReport>: the read-only damage
// map (the browser twin of `mkvgo salvage --dry-run`). It walks the file the
// way a repair would - surgical recovery, damaged ranges with byte offsets
// and approximate presentation times, clean-cut cost when opts.cleanCut is
// set - and writes nothing, so a local file can be diagnosed before any
// upload. The repair operations themselves (salvage, reindex, retime,
// rollback) stay out of WASM by design: they rewrite or patch files, and
// browser inputs are read-only Blobs.
func mapDamageJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("mapDamage: missing input") })
	}
	input := args[0]
	opts := optArg(args, 1)
	ctx, release := signalContext(opts)
	cleanCut := opts.Truthy() && opts.Get("cleanCut").Truthy()
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("mapDamage: %w", err)
		}
		report, err := ops.MapDamage(ctx, "in", mkv.Options{FS: fs, CleanCut: cleanCut})
		if err != nil {
			return nil, err
		}
		return toJSObject(report)
	})
}

// openWatermarkJS(a, b, opts?) -> Promise<WatermarkHandle> for forensic A/B
// session watermarking. a and b are two GOP-aligned encodes of one title
// (Uint8Array or Blob/File). The handle serves shared playlists plus per-segment
// bytes routed to variant A or B by a per-viewer bit: segment(n, fromB) or
// segmentForPattern(n, patternBytes). The caller owns the code assignment.
func openWatermarkJS(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return promise(func() (any, error) {
			return nil, fmt.Errorf("openWatermark: needs two inputs (variant A, variant B)")
		})
	}
	inputs := []js.Value{args[0], args[1]}
	opts := readRemuxOpts(args, 2)
	openCtx, openRelease := signalContext(optArg(args, 2))
	return promise(func() (any, error) {
		defer openRelease()
		fs, paths, err := multiSourceFS(inputs)
		if err != nil {
			return nil, err
		}
		opts.FS = fs
		wm, err := mp4.PlanWatermark(openCtx, paths[0], paths[1], opts)
		if err != nil {
			return nil, err
		}

		h := js.Global().Get("Object").New()
		h.Set("numSegments", wm.NumSegments())
		h.Set("masterPlaylist", toUint8Array(wm.MasterPlaylist()))
		h.Set("mediaPlaylist", toUint8Array(wm.MediaPlaylist()))
		h.Set("init", toUint8Array(wm.InitSegment()))

		segmentFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("segment: missing index") })
			}
			n := rargs[0].Int()
			fromB := len(rargs) > 1 && rargs[1].Truthy()
			ctx, release := signalContext(optArg(rargs, 2))
			return promise(func() (any, error) {
				defer release()
				data, err := wm.Segment(ctx, n, fromB)
				if err != nil {
					return nil, err
				}
				return toUint8Array(data), nil
			})
		})
		patternFn := js.FuncOf(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 2 || !isUint8(rargs[1]) {
				return promise(func() (any, error) {
					return nil, fmt.Errorf("segmentForPattern: needs an index and a Uint8Array pattern")
				})
			}
			n := rargs[0].Int()
			pattern := toGoBytes(rargs[1])
			ctx, release := signalContext(optArg(rargs, 2))
			return promise(func() (any, error) {
				defer release()
				data, err := wm.SegmentForPattern(ctx, n, pattern)
				if err != nil {
					return nil, err
				}
				return toUint8Array(data), nil
			})
		})
		var closeFn js.Func
		closeFn = js.FuncOf(func(js.Value, []js.Value) any {
			segmentFn.Release()
			patternFn.Release()
			closeFn.Release()
			return nil
		})
		h.Set("segment", segmentFn)
		h.Set("segmentForPattern", patternFn)
		h.Set("close", closeFn)
		return h, nil
	})
}
