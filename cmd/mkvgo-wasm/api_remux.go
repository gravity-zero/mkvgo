//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall/js"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// remuxOpts reads the optional remux options object.
func readRemuxOpts(args []js.Value, idx int) (mp4.Options, error) {
	var o mp4.Options
	if len(args) <= idx || args[idx].Type() != js.TypeObject {
		return o, nil
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
			id, err := intArg(k.Index(i), "keepTracks: each track id")
			if err != nil {
				return o, err
			}
			o.KeepTracks = append(o.KeepTracks, uint64(id))
		}
	}
	if s := v.Get("subOffsetMs"); s.Type() == js.TypeNumber {
		o.SubtitleOffsetMs = int64(s.Float())
	}
	o.SynthesizeIndex = v.Get("synthesizeIndex").Truthy()
	// windowCacheBytes: what an on-demand plan may hold for the renditions of a
	// window nobody has collected. Omit it and the plan sizes itself from the
	// source; a negative value turns the sharing off (every rendition then
	// re-walks its own window, so a viewer's video and audio read the source
	// twice - through the Blob, that is twice the range requests).
	if s := v.Get("windowCacheBytes"); s.Type() == js.TypeNumber {
		o.WindowCacheBytes = int64(s.Float())
	}
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
	return o, nil
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
	if b := c.Get("inlineKey"); b.Type() == js.TypeBoolean {
		out.InlineKey = b.Bool()
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
	return remuxCall(args, "out.mp4", func(ctx context.Context, o mp4.Options) error {
		return mp4.RemuxToMP4(ctx, "in", "out.mp4", o)
	})
}

// remuxFromMP4JS(input: Uint8Array, opts?) → Promise<{data, droppedTracks}>
func remuxFromMP4JS(_ js.Value, args []js.Value) any {
	return remuxCall(args, "out.mkv", func(ctx context.Context, o mp4.Options) error {
		return mp4.RemuxFromMP4(ctx, "in", "out.mkv", o)
	})
}

// remuxToWebMJS(input: Uint8Array) → Promise<{data, droppedTracks}>
func remuxToWebMJS(_ js.Value, args []js.Value) any {
	return remuxCall(args, "out.webm", func(ctx context.Context, o mp4.Options) error {
		return matroska.RemuxToWebM(ctx, "in", "out.webm", matroska.Options{FS: o.FS})
	})
}

// remuxCall factors the shared shape: the input as "in" (see inputFS), run
// the operation, return the output plus the dropped tracks.
func remuxCall(args []js.Value, out string, run func(context.Context, mp4.Options) error) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("missing input") })
	}
	input := args[0]
	opts, err := readRemuxOpts(args, 1)
	if err != nil {
		return rejected(err)
	}
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		m, fs, size, err := inputFS(input)
		if err != nil {
			return nil, err
		}
		// A remux writes about what it reads: room for that keeps the output from growing by copies.
		m.Reserve(out, int(size+size/32+1<<20))
		var dropped []mp4.DroppedTrack
		opts.FS = fs
		opts.OnDrop = func(d mp4.DroppedTrack) { dropped = append(dropped, d) }
		if err := run(ctx, opts); err != nil {
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
	opts, err := readRemuxOpts(args, 1)
	if err != nil {
		return rejected(err)
	}
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		m, fs, _, err := inputFS(input)
		if err != nil {
			return nil, err
		}
		var dropped []mp4.DroppedTrack
		opts.FS = fs
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

// extractSubtitleVTTJS(input: Uint8Array | Blob | File, trackId: number) → Promise<string>
func extractSubtitleVTTJS(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return promise(func() (any, error) { return nil, fmt.Errorf("extractSubtitleVTT: need (input, trackId)") })
	}
	input := args[0]
	id, err := intArg(args[1], "extractSubtitleVTT: the track id")
	if err != nil {
		return rejected(err)
	}
	trackID := uint64(id)
	return promise(func() (any, error) {
		fs, err := singleSourceFS(input) // a Blob/File is read by ranges, as every Blob-backed export
		if err != nil {
			return nil, fmt.Errorf("extractSubtitleVTT: %w", err)
		}
		format, err := sniffSource(fs)
		if err != nil {
			return nil, err
		}
		var out strings.Builder
		switch format {
		case "mkv":
			err = matroska.ExtractSubtitleWebVTT(context.Background(), "in", trackID, &out,
				matroska.Options{FS: fs})
		case "mp4":
			err = mp4.ExtractSubtitleWebVTT(context.Background(), "in", trackID, &out,
				mp4.Options{FS: fs})
		}
		if errors.Is(err, matroska.ErrSourceTruncated) {
			return nil, partialResult{err, out.String()}
		}
		if err != nil {
			return nil, err
		}
		return out.String(), nil
	})
}
