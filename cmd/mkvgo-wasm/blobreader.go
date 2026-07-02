//go:build js && wasm

package main

import (
	"fmt"
	"io"
	"syscall/js"
)

// blobReader adapts a JS Blob/File to io.ReadSeeker through ranged
// blob.slice(...).arrayBuffer() calls, cached in a window so the many small
// reads a parser makes don't each cross the JS boundary. Head-only probing of
// an arbitrarily large file stays head-only: only the windows actually read
// are ever transferred.
type blobReader struct {
	blob js.Value
	size int64
	pos  int64
	buf  []byte // cached window
	off  int64  // buf's start offset in the blob
}

const blobWindow = 512 << 10

func newBlobReader(blob js.Value) *blobReader {
	return &blobReader{blob: blob, size: int64(blob.Get("size").Float())}
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.pos < r.off || r.pos >= r.off+int64(len(r.buf)) {
		if err := r.fetch(r.pos); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.pos-r.off:])
	r.pos += int64(n)
	return n, nil
}

func (r *blobReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, fmt.Errorf("blob: bad whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("blob: negative seek %d", next)
	}
	r.pos = next
	return next, nil
}

// fetch loads the window starting at off. Blocks the calling goroutine on the
// arrayBuffer promise — legal here because every export runs its work on its
// own goroutine, leaving the JS event loop free to complete the read.
func (r *blobReader) fetch(off int64) error {
	end := off + blobWindow
	if end > r.size {
		end = r.size
	}
	ab, err := await(r.blob.Call("slice", off, end).Call("arrayBuffer"))
	if err != nil {
		return fmt.Errorf("blob read [%d:%d): %w", off, end, err)
	}
	u8 := js.Global().Get("Uint8Array").New(ab)
	r.buf = make([]byte, u8.Get("length").Int())
	js.CopyBytesToGo(r.buf, u8)
	r.off = off
	return nil
}

// await blocks the current goroutine until the JS promise settles.
func await(p js.Value) (js.Value, error) {
	done := make(chan struct{})
	var result js.Value
	var errMsg string
	then := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) > 0 {
			result = args[0]
		}
		close(done)
		return nil
	})
	catch := js.FuncOf(func(_ js.Value, args []js.Value) any {
		errMsg = "promise rejected"
		if len(args) > 0 {
			errMsg = args[0].Call("toString").String()
		}
		close(done)
		return nil
	})
	defer then.Release()
	defer catch.Release()
	p.Call("then", then).Call("catch", catch)
	<-done
	if errMsg != "" {
		return js.Value{}, fmt.Errorf("%s", errMsg)
	}
	return result, nil
}
