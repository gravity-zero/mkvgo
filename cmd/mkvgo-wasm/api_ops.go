//go:build js && wasm

package main

import (
	"fmt"
	"io"
	"syscall/js"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mp4"
)

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
// even on a Blob (a handful of ranged reads: the head, then the SeekHead's Cues
// or, failing that, one window at the file end - never a cluster walk). It spots
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

// trackEndsJS(input, opts?) -> Promise<TrackEndsReport>: where each track's
// content really ends - the statistics DURATION tags when they describe the
// file (one head-only read), else a bounded header-only walk of the tail from
// a cue a window before the end (ranged reads on a Blob, payloads skipped):
// the picture's end and any audio track's shortfall against it.
func trackEndsJS(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return promise(func() (any, error) { return nil, fmt.Errorf("trackEnds: missing input") })
	}
	input := args[0]
	ctx, release := signalContext(optArg(args, 1))
	return promise(func() (any, error) {
		defer release()
		fs, err := singleSourceFS(input)
		if err != nil {
			return nil, fmt.Errorf("trackEnds: %w", err)
		}
		report, err := ops.TrackEnds(ctx, "in", mkv.Options{FS: fs})
		if err != nil {
			return nil, err
		}
		return toJSObject(report)
	})
}

// diagnoseJS(input, opts?) -> Promise<Diagnosis>: one-call triage with a
// remedy per finding, routed by the input's first bytes like the CLI.
// Matroska/WebM: seek-index health, per-track audio start delays,
// declared-size coherence, tolerant walk only when the sizes disagree.
// MP4/MOV: head-only box-layout truncation, missing moov, trailing junk,
// per-track edit-list audio delays. Same Diagnosis shape either way.
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
		src, err := fs.DoOpen("in")
		if err != nil {
			return nil, fmt.Errorf("diagnose: %w", err)
		}
		head := make([]byte, 8)
		_, herr := io.ReadFull(src, head)
		src.Close()
		if herr != nil {
			return nil, fmt.Errorf("diagnose: read head: %w", herr)
		}
		format, err := sniffFormat(head)
		if err != nil {
			return nil, fmt.Errorf("diagnose: %w", err)
		}
		var d *mkv.Diagnosis
		if format == "mkv" {
			d, err = ops.Diagnose(ctx, "in", mkv.Options{FS: fs})
		} else {
			d, err = mp4.Diagnose(ctx, "in", mp4.Options{FS: fs})
		}
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
	opts, err := readRemuxOpts(args, 1)
	if err != nil {
		return rejected(err)
	}
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

		segmentFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("segment: missing index") })
			}
			n, err := intArg(rargs[0], "the segment index")
			if err != nil {
				return rejected(err)
			}
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
		patternFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 2 || !isUint8(rargs[1]) {
				return promise(func() (any, error) {
					return nil, fmt.Errorf("segmentForPattern: needs an index and a Uint8Array pattern")
				})
			}
			n, err := intArg(rargs[0], "the segment index")
			if err != nil {
				return rejected(err)
			}
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
		distinctFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("distinct: missing index") })
			}
			n, err := intArg(rargs[0], "the segment index")
			if err != nil {
				return rejected(err)
			}
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
		closeFn = jsFunc(func(js.Value, []js.Value) any {
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
	opts, err := readRemuxOpts(args, 2)
	if err != nil {
		return rejected(err)
	}
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

		segmentFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 1 {
				return promise(func() (any, error) { return nil, fmt.Errorf("segment: missing index") })
			}
			n, err := intArg(rargs[0], "the segment index")
			if err != nil {
				return rejected(err)
			}
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
		patternFn := jsFunc(func(_ js.Value, rargs []js.Value) any {
			if len(rargs) < 2 || !isUint8(rargs[1]) {
				return promise(func() (any, error) {
					return nil, fmt.Errorf("segmentForPattern: needs an index and a Uint8Array pattern")
				})
			}
			n, err := intArg(rargs[0], "the segment index")
			if err != nil {
				return rejected(err)
			}
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
		closeFn = jsFunc(func(js.Value, []js.Value) any {
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
