package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// cancelFS counts reads and cancels after the at-th one; reads made after the cancel are counted apart.
type cancelFS struct {
	base         *mkv.FS
	at           int
	cancel       func()
	reads, after int
	opens        int
}

type cancelFile struct {
	mkv.ReadSeekCloser
	fs *cancelFS
}

func (f *cancelFile) Read(p []byte) (int, error) {
	c := f.fs
	if c.at > 0 && c.reads >= c.at {
		c.after++
	}
	n, err := f.ReadSeekCloser.Read(p)
	c.reads++
	if c.reads == c.at {
		c.cancel()
	}
	return n, err
}

func (c *cancelFS) fs() *mkv.FS {
	fs := *c.base
	fs.Open = func(p string) (mkv.ReadSeekCloser, error) {
		c.opens++
		f, err := c.base.Open(p)
		if err != nil {
			return nil, err
		}
		return &cancelFile{ReadSeekCloser: f, fs: c}, nil
	}
	return &fs
}

// TestPlanHLSMP4CancelledBeforeOrDuringPlanRejects cancels a fragmented and a progressive MP4 plan before the call, then after each of its reads in turn: every plan returns context.Canceled itself and reads (almost) nothing more.
func TestPlanHLSMP4CancelledBeforeOrDuringPlanRejects(t *testing.T) {
	frag, _ := makeFragmentedMP4(t)
	src, _ := buildLacedFixture(t)
	dst := filepath.Join(t.TempDir(), "prog.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	prog, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{{"fragmented", frag}, {"progressive", prog}} {
		m := mkv.NewMemFS()
		m.Put("in.mp4", tc.data)
		pre, cancel := context.WithCancel(context.Background())
		cancel()
		c := &cancelFS{base: m.FS()}
		if _, err := PlanHLS(pre, "in.mp4", Options{SegmentMs: 2000, FS: c.fs()}); err != context.Canceled || c.opens != 0 {
			t.Fatalf("%s, cancelled before the call: err = %v, %d opens, want context.Canceled and no open", tc.name, err, c.opens)
		}
		ref := &cancelFS{base: m.FS()}
		if _, err := PlanHLS(context.Background(), "in.mp4", Options{SegmentMs: 2000, FS: ref.fs()}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		maxAfter := 0
		for at := 1; at <= ref.reads; at++ {
			ctx, cancel := context.WithCancel(context.Background())
			c := &cancelFS{base: m.FS(), at: at, cancel: cancel}
			_, err := PlanHLS(ctx, "in.mp4", Options{SegmentMs: 2000, FS: c.fs()})
			cancel()
			if err != context.Canceled {
				t.Fatalf("%s, cancel after read %d/%d: err = %v, want context.Canceled itself", tc.name, at, ref.reads, err)
			}
			maxAfter = max(maxAfter, c.after)
		}
		if maxAfter > 2 {
			t.Errorf("%s: up to %d reads after the cancel, want at most 2", tc.name, maxAfter)
		}
		t.Logf("%s: %d reads per plan, at most %d after a cancel", tc.name, ref.reads, maxAfter)
	}
}

// TestPlanHLSMKVCancelledBeforeCall checks the Matroska path returns the bare context error without opening the source.
func TestPlanHLSMKVCancelledBeforeCall(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 50; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	src := buildPlanFixture(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		gblocks, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PlanHLS(ctx, src, Options{SegmentMs: 2000}); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled itself", err)
	}
}
