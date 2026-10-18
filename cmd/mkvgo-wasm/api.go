//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"syscall/js"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"

	"github.com/gravity-zero/mkvgo/mkv"
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

// intArg reads v as an integer, refusing anything that is not a number: Value.Int panics on a string, outside any
// promise, and the panic ends the Go program.
func intArg(v js.Value, what string) (int, error) {
	if v.Type() != js.TypeNumber {
		return 0, fmt.Errorf("%s must be a number, got %s", what, v.Type())
	}
	return v.Int(), nil
}

// rejected is a promise that rejects with err.
func rejected(err error) any { return promise(func() (any, error) { return nil, err }) }

// guarded is fn turning any panic into a rejected promise: what a caller passes is read before fn's promise exists,
// and a panic there, outside any recover, ends the Go program.
func guarded(fn func(js.Value, []js.Value) any) func(js.Value, []js.Value) any {
	return func(this js.Value, args []js.Value) (out any) {
		defer func() {
			if r := recover(); r != nil {
				out = rejected(fmt.Errorf("mkvgo: invalid arguments: %v", r))
			}
		}()
		return fn(this, args)
	}
}

// jsFunc is js.FuncOf over guarded(fn): the form every function handed to JavaScript takes.
func jsFunc(fn func(js.Value, []js.Value) any) js.Func { return js.FuncOf(guarded(fn)) }

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
				e := jsError(err.Error())
				var pe partialResult
				if errors.As(err, &pe) {
					e.Set("partial", pe.value)
				}
				reject.Invoke(e)
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

// partialResult is a failure that still hands the caller what was produced, as the rejection's partial.
type partialResult struct {
	error
	value any
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
// sniffSource reads the head of the source "in" of fs and names its container.
func sniffSource(fs *mkv.FS) (string, error) {
	f, err := fs.DoOpen("in")
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	return sniffFormat(head[:n])
}

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
	// FrameRate is the Track's, else its EstimatedFrameRate with FrameRateEstimated set.
	FrameRate           *float64 `json:"frame_rate,omitempty"`
	FrameRateEstimated  bool     `json:"frame_rate_estimated,omitempty"`
	EstimatedFrameRate  *float64 `json:"estimated_frame_rate,omitempty"` // folded into frame_rate
	CodecLongName       string   `json:"codec_long_name,omitempty"`
	ChannelLayout       string   `json:"channel_layout,omitempty"`
	AvgFrameRate        float64  `json:"avg_frame_rate,omitempty"`
	SampleAspectRatio   string   `json:"sample_aspect_ratio,omitempty"`
	DisplayAspectRatio  string   `json:"display_aspect_ratio,omitempty"`
	ColorSpaceName      string   `json:"color_space_name,omitempty"`
	ColorTransferName   string   `json:"color_transfer_name,omitempty"`
	ColorPrimariesName  string   `json:"color_primaries_name,omitempty"`
	ColorRangeName      string   `json:"color_range_name,omitempty"`
	HDRFormat           string   `json:"hdr_format,omitempty"`
	StereoModeName      string   `json:"stereo_mode_name,omitempty"`
	ResolvedLanguage    string   `json:"resolved_language,omitempty"`
	EffectiveSampleRate float64  `json:"effective_sample_rate,omitempty"`
}

// trackJSONOf fills every derived field from the Track's methods.
func trackJSONOf(c *matroska.Container, t matroska.Track) trackJSON {
	fr, estimated := t.FrameRate, t.FrameRate == nil && t.EstimatedFrameRate != nil
	if estimated {
		fr = t.EstimatedFrameRate
	}
	return trackJSON{Track: t, FrameRate: fr, FrameRateEstimated: estimated,
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
		ResolvedLanguage:    c.TrackLanguage(&t),
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
		// Chapters, tags and the attachment list (no payload) complete the CLI -json shape.
		ro := []matroska.ReadOption{matroska.WithChapters(), matroska.WithTags(),
			matroska.WithAttachments(), matroska.WithoutAttachmentData()}
		if opts.keyframes {
			ro = append(ro, matroska.WithKeyframeIndex())
		}
		if opts.bitrate {
			ro = append(ro, matroska.WithBitrate())
		}
		if opts.inBandColour {
			ro = append(ro, matroska.WithInBandColourFallback(), matroska.WithInBandChannels())
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
		res.Tracks[i] = trackJSONOf(res.Container, t)
	}
	return toJSObject(res)
}

// toJSObject marshals v to JSON and parses it on the JS side, yielding a plain
// object (numbers/strings/arrays) the caller uses directly; 64-bit UIDs stay exact as strings.
func toJSObject(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return js.Global().Get("JSON").Call("parse", string(jsSafeJSON(raw))), nil
}
