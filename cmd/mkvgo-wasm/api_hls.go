//go:build js && wasm

package main

import (
	"fmt"
	"syscall/js"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

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
	opts, err := readRemuxOpts(args, 1)
	if err != nil {
		return rejected(err)
	}
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

		resourceFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
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
		segmentFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("segment: missing index") })
			}
			n, err := intArg(rargs[0], "segment: the index")
			if err != nil {
				return rejected(err)
			}
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
		closeFn = jsFunc(func(js.Value, []js.Value) any {
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
	opts, err := readRemuxOpts(args, 1)
	if err != nil {
		return rejected(err)
	}
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

		resourceFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
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
		closeFn = jsFunc(func(js.Value, []js.Value) any {
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
	opts, err := readRemuxOpts(args, 1)
	if err != nil {
		return rejected(err)
	}
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

		resourceFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
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
		closeFn = jsFunc(func(js.Value, []js.Value) any {
			resourceFn.Release()
			closeFn.Release()
			return nil
		})
		h.Set("resource", resourceFn)
		h.Set("close", closeFn)
		return h, nil
	})
}
