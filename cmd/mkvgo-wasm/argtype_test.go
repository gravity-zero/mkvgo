//go:build js && wasm

package main

import (
	"syscall/js"
	"testing"
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
