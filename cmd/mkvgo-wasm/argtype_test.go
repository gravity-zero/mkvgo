//go:build js && wasm

package main

import (
	"errors"
	"syscall/js"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestWrongArgumentTypesRejectInsteadOfExiting pins that a string where a number belongs is a rejected promise: a
// panic outside the promise ended the Go program, and every later call failed.
func TestWrongArgumentTypesRejectInsteadOfExiting(t *testing.T) {
	input := js.Global().Get("Uint8Array").New(4)
	p := js.ValueOf(extractSubtitleVTTJS(js.Undefined(), []js.Value{input, js.ValueOf("2")}))
	if p.Type() != js.TypeObject {
		t.Fatalf("extractSubtitleVTT(input, \"2\") returned %v, want a promise", p)
	}
	ignore := js.FuncOf(func(js.Value, []js.Value) any { return nil })
	defer ignore.Release()
	p.Call("catch", ignore) // the rejection is the answer; unhandled, it would end the test runner
	opts := js.Global().Get("Object").New()
	opts.Set("keepTracks", js.ValueOf([]any{"1"}))
	if _, err := readRemuxOpts([]js.Value{input, opts}, 1); err == nil {
		t.Error(`keepTracks: ["1"] was taken`)
	}
	if _, err := intArg(js.ValueOf("3"), "segment index"); err == nil {
		t.Error(`intArg("3") was taken as a number`)
	}
	if n, err := intArg(js.ValueOf(3), "segment index"); err != nil || n != 3 {
		t.Errorf("intArg(3) = %d, %v", n, err)
	}
}

// TestBadOptionsRejectInsteadOfExiting pins that an option a caller gets wrong (a key passed as an ArrayBuffer, a
// keys entry that is null, a signal that is not an AbortSignal) is a rejected promise from every exported function.
func TestBadOptionsRejectInsteadOfExiting(t *testing.T) {
	input := js.Global().Get("Uint8Array").New(4)
	cenc := js.Global().Get("Object").New()
	cenc.Set("key", js.Global().Get("ArrayBuffer").New(16))
	cenc.Set("keyId", js.Global().Get("Uint8Array").New(16))
	cenc.Set("iv", js.Global().Get("Uint8Array").New(8))
	withCENC := js.Global().Get("Object").New()
	withCENC.Set("cenc", cenc)
	enc := js.Global().Get("Object").New()
	enc.Set("rotateEverySegments", 2)
	enc.Set("keys", js.ValueOf([]any{nil}))
	withKeys := js.Global().Get("Object").New()
	withKeys.Set("encrypt", enc)
	withSignal := js.Global().Get("Object").New()
	withSignal.Set("signal", js.Global().Get("Object").New())
	ignore := js.FuncOf(func(js.Value, []js.Value) any { return nil })
	defer ignore.Release()
	for name, opts := range map[string]js.Value{"cenc key": withCENC, "null key": withKeys, "signal": withSignal} {
		p := js.ValueOf(guarded(openHLSJS)(js.Undefined(), []js.Value{input, opts}))
		if p.Type() != js.TypeObject {
			t.Fatalf("%s: openHLS returned %v, want a promise", name, p)
		}
		p.Call("catch", ignore)
	}
}

// TestNumericOptionsAreChecked: NaN, an infinity, a fraction or a negative id passed as an option converted to an
// integer as nothing in particular (Value.Int of NaN, an id of 2^64-1); each is refused by name.
func TestNumericOptionsAreChecked(t *testing.T) {
	input := js.Global().Get("Uint8Array").New(4)
	nan := js.Global().Get("NaN")
	inf := js.Global().Get("Infinity")
	for name, set := range map[string]func(js.Value){
		"segmentSeconds NaN":    func(o js.Value) { o.Set("segmentSeconds", nan) },
		"segmentSeconds 1e300":  func(o js.Value) { o.Set("segmentSeconds", 1e300) },
		"segmentSeconds 0":      func(o js.Value) { o.Set("segmentSeconds", 0) },
		"subOffsetMs Infinity":  func(o js.Value) { o.Set("subOffsetMs", inf) },
		"subOffsetMs 1.5":       func(o js.Value) { o.Set("subOffsetMs", 1.5) },
		"windowCacheBytes NaN":  func(o js.Value) { o.Set("windowCacheBytes", nan) },
		"keepTracks -1":         func(o js.Value) { o.Set("keepTracks", js.ValueOf([]any{-1})) },
		"keepTracks 2.5":        func(o js.Value) { o.Set("keepTracks", js.ValueOf([]any{2.5})) },
		"audioShiftMs NaN":      func(o js.Value) { m := js.Global().Get("Object").New(); m.Set("2", nan); o.Set("audioShiftMs", m) },
		"audioShiftMs 1e15":     func(o js.Value) { m := js.Global().Get("Object").New(); m.Set("2", 1e15); o.Set("audioShiftMs", m) },
		"segmentSeconds string": func(o js.Value) { o.Set("segmentSeconds", "6") },
	} {
		opts := js.Global().Get("Object").New()
		set(opts)
		if _, err := readRemuxOpts([]js.Value{input, opts}, 1); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	ok := js.Global().Get("Object").New()
	ok.Set("segmentSeconds", 4)
	ok.Set("subOffsetMs", -250)
	ok.Set("keepTracks", js.ValueOf([]any{1, 2}))
	if o, err := readRemuxOpts([]js.Value{input, ok}, 1); err != nil || o.SegmentMs != 4000 || o.SubtitleOffsetMs != -250 || len(o.KeepTracks) != 2 {
		t.Errorf("valid options: %+v, %v", o, err)
	}
	if _, err := intArg(nan, "segment index"); err == nil {
		t.Error("intArg(NaN) was taken")
	}
}

// TestSniffFormatSharesTheReaders: the wasm sniffer kept its own list of opening boxes, missing moof, sidx, uuid and
// others the readers take, and read an MPEG-TS as "unrecognised" where every reader names it.
func TestSniffFormatSharesTheReaders(t *testing.T) {
	for _, typ := range []string{"moof", "sidx", "uuid", "pnot", "emsg"} {
		head := append([]byte{0, 0, 0, 16}, typ...)
		if f, err := sniffFormat(head); err != nil || f != "mp4" {
			t.Errorf("a source opening on %q: %q, %v", typ, f, err)
		}
	}
	ts := make([]byte, 4+2*192+1)
	for _, off := range []int{0, 188, 376} {
		ts[off] = 0x47
	}
	if _, err := sniffFormat(ts); !errors.Is(err, mkv.ErrMPEGTS) {
		t.Errorf("an MPEG-TS: %v, want mkv.ErrMPEGTS", err)
	}
}

// TestClosedHandleRejects: a method called after close() - through the handle or through a copy taken before
// (const {resource, close} = h) - reached a released function, which returns undefined: an await of it went on with
// nothing. It rejects by name, close may be called again, and what the method called is dropped.
func TestClosedHandleRejects(t *testing.T) {
	h := js.Global().Get("Object").New()
	called := false
	resource := newMethod(func(js.Value, []js.Value) any { called = true; return nil })
	var closeM *handleMethod
	closeM = newMethod(func(js.Value, []js.Value) any {
		closeHandle(h, map[string]*handleMethod{"resource": resource}, closeM)
		return nil
	})
	h.Set("resource", resource.Func)
	h.Set("close", closeM.Func)
	copied, copiedClose := h.Get("resource"), h.Get("close")
	h.Call("close")
	ignore := js.FuncOf(func(js.Value, []js.Value) any { return nil })
	defer ignore.Release()
	for _, p := range []js.Value{h.Call("resource", "seg00001.m4s"), copied.Invoke("seg00001.m4s")} {
		if p.Type() != js.TypeObject || p.Get("then").Type() != js.TypeFunction {
			t.Fatalf("resource after close returned %v, want a promise", p)
		}
		p.Call("catch", ignore)
	}
	h.Call("close")      // again: a no-op
	copiedClose.Invoke() // through the copy too
	if called {
		t.Error("the closed method ran")
	}
	if resource.inner.Load() != nil {
		t.Error("the closed method still holds what it called")
	}
}
