//go:build js && wasm

package main

import (
	"fmt"
	"io"
	"syscall/js"
)

// blobReader adapts a JS Blob/File to io.ReadSeeker through ranged
// blob.slice(...).arrayBuffer() calls, cached in a few windows so the many small
// reads a parser makes don't each cross the JS boundary. Head-only probing of
// an arbitrarily large file stays head-only: only the windows actually read
// are ever transferred.
type blobReader struct {
	blob  js.Value
	size  int64
	pos   int64
	cache blobCache
}

func newBlobReader(blob js.Value) *blobReader {
	return &blobReader{blob: blob, size: int64(blob.Get("size").Float())}
}

func (r *blobReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	n, err := r.cache.read(p, r.pos, r.size, r.fetch)
	r.pos += int64(n)
	return n, err
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

// ReadAt reads len(p) bytes at off with one exact Blob slice (io.ReaderAt), leaving the windows and the position alone.
func (r *blobReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.size-off))
	buf, err := r.fetch(p[:n], off)
	if err != nil {
		return 0, err
	}
	if len(buf) < len(p) {
		return len(buf), io.EOF
	}
	return len(buf), nil
}

// Free reports whether n bytes at off lie in a window already read, so reading them costs no Blob read.
func (r *blobReader) Free(off, n int64) bool { return r.cache.holds(off, n) }

// Close satisfies mkv.ReadSeekCloser; a Blob slice holds no OS resource.
func (r *blobReader) Close() error { return nil }

// fetch fills dst from the Blob at off. Blocks the calling goroutine on the
// arrayBuffer promise - legal here because every export runs its work on its
// own goroutine, leaving the JS event loop free to complete the read.
func (r *blobReader) fetch(dst []byte, off int64) ([]byte, error) {
	end := off + int64(len(dst))
	ab, err := await(r.blob.Call("slice", off, end).Call("arrayBuffer"))
	if err != nil {
		return nil, fmt.Errorf("blob read [%d:%d): %w", off, end, err)
	}
	u8 := js.Global().Get("Uint8Array").New(ab)
	n := js.CopyBytesToGo(dst, u8)
	return dst[:n], nil
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
