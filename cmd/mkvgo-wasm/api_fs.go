//go:build js && wasm

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"syscall/js"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

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

// inputFS is a MemFS for an operation's outputs whose "in" is the input
// itself, never copied into it: a Blob/File read through ranged slices, or a
// Uint8Array copied once into Go and read in place.
func inputFS(input js.Value) (*mkv.MemFS, *mkv.FS, int64, error) {
	rs, err := inputReadSeeker(input)
	if err != nil {
		return nil, nil, 0, err
	}
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, 0, err
	}
	open := func() mkv.ReadSeekCloser { return newBlobReader(input) }
	if b, ok := rs.(*bytes.Reader); ok {
		open = func() mkv.ReadSeekCloser { return nopCloser{io.NewSectionReader(b, 0, size)} }
	}
	m := mkv.NewMemFS()
	fs := m.FS()
	openOut, statOut := fs.Open, fs.Stat
	fs.Open = func(p string) (mkv.ReadSeekCloser, error) {
		if p == "in" {
			return open(), nil
		}
		return openOut(p)
	}
	fs.Stat = func(p string) (os.FileInfo, error) {
		if p == "in" {
			return blobInfo{name: p, size: size}, nil
		}
		return statOut(p)
	}
	return m, fs, size, nil
}

type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

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

// isUint8 reports whether v is a Uint8Array: an ArrayBuffer or another typed array has no bytes toGoBytes can copy.
func isUint8(v js.Value) bool {
	return v.Type() == js.TypeObject && v.InstanceOf(js.Global().Get("Uint8Array"))
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
