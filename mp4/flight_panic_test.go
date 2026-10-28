package mp4

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// panicFS reads from base, and panics on a read while armed.
type panicFS struct {
	base  *mkv.FS
	armed atomic.Bool
}

type panicFile struct {
	mkv.ReadSeekCloser
	fs *panicFS
}

func (f *panicFile) Read(p []byte) (int, error) {
	if f.fs.armed.Load() {
		panic("forced read panic")
	}
	return f.ReadSeekCloser.Read(p)
}

func (f *panicFile) ReadAt(p []byte, off int64) (int, error) {
	if f.fs.armed.Load() {
		panic("forced read panic")
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(f.ReadSeekCloser, p)
}

func (c *panicFS) fs() *mkv.FS {
	fs := *c.base
	fs.Open = func(p string) (mkv.ReadSeekCloser, error) {
		f, err := c.base.Open(p)
		if err != nil {
			return nil, err
		}
		return &panicFile{ReadSeekCloser: f, fs: c}, nil
	}
	return &fs
}

// TestFlightLeaderPanicReleasesTheWaiters: a window or table build that panicked left its flight registered, never
// finished: every later request for that segment joined it and waited as long as its context lasted. The panic is
// the leader's error now, and the next request builds afresh.
func TestFlightLeaderPanicReleasesTheWaiters(t *testing.T) {
	src, _ := buildLacedFixture(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, via := range []string{"Resource", "Open"} {
		m := mkv.NewMemFS()
		m.Put("in.mkv", data)
		pf := &panicFS{base: m.FS()}
		plan, err := PlanHLS(context.Background(), "in.mkv", Options{SegmentMs: 2000, FS: pf.fs()})
		if err != nil {
			t.Fatal(err)
		}
		serve := func(ctx context.Context) (b []byte, err error) {
			if via == "Resource" {
				b, _, err = plan.Resource(ctx, "seg00001.m4s")
				return b, err
			}
			h, err := plan.Open(ctx, "seg00001.m4s")
			if err != nil {
				return nil, err
			}
			defer h.Close()
			var out bytes.Buffer
			_, err = h.WriteTo(ctx, &out)
			return out.Bytes(), err
		}
		pf.armed.Store(true)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: the build's panic reached the caller: %v", via, r)
				}
			}()
			if _, err := serve(context.Background()); err == nil {
				t.Errorf("%s: a build that panicked served the segment", via)
			}
		}()
		pf.armed.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if b, err := serve(ctx); err != nil || len(b) == 0 {
			t.Errorf("%s: the request after the panic: %d bytes, %v", via, len(b), err)
		}
		cancel()
	}
}
