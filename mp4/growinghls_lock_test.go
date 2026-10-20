package mp4

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// gatedFile holds a read starting in [from, to) until the gate opens; to 0 bounds nothing.
type gatedFile struct {
	mkv.ReadSeekCloser
	pos     int64
	from    int64
	to      int64
	armed   *atomic.Bool
	reached chan struct{}
	open    chan struct{}
	once    *sync.Once
}

func (f *gatedFile) Read(b []byte) (int, error) {
	if f.armed.Load() && f.pos >= f.from && (f.to == 0 || f.pos < f.to) {
		f.once.Do(func() { close(f.reached) })
		<-f.open
	}
	n, err := f.ReadSeekCloser.Read(b)
	f.pos += int64(n)
	return n, err
}

func (f *gatedFile) Seek(off int64, whence int) (int64, error) {
	pos, err := f.ReadSeekCloser.Seek(off, whence)
	f.pos = pos
	return pos, err
}

// TestGrowingHLSServesWhileRefreshReads: a Refresh reading new clusters held every request until it was done, the
// plan's lock taken over the scan's I/O; requests now run on, the lock taken only to publish.
func TestGrowingHLSServesWhileRefreshReads(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	fs := *m.FS()
	var armed atomic.Bool
	reached, open, once := make(chan struct{}), make(chan struct{}), &sync.Once{}
	base := fs.Open
	fs.Open = func(path string) (mkv.ReadSeekCloser, error) {
		f, err := base(path)
		if err != nil {
			return nil, err
		}
		return &gatedFile{ReadSeekCloser: f, from: clusters[5].start, armed: &armed, reached: reached, open: open, once: once}, nil
	}
	fs.OpenContext = nil
	m.Put("in.mkv", data[:clusters[5].start])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: &fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() == 0 {
		t.Fatal("no segment published before the gate")
	}
	want, err := plan.Segment(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	m.Put("in.mkv", data[:clusters[8].end])
	armed.Store(true)
	refreshed := make(chan error, 1)
	go func() { _, err := plan.Refresh(context.Background()); refreshed <- err }()
	<-reached // the Refresh is reading the new clusters

	served := make(chan error, 1)
	go func() {
		got, err := plan.Segment(context.Background(), 0)
		if err == nil && string(got) != string(want) {
			err = io.ErrUnexpectedEOF
		}
		_, _, rerr := plan.Resource(context.Background(), "playlist.m3u8")
		if err == nil {
			err = rerr
		}
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("a request while Refresh reads: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a request waited on the Refresh reading new clusters")
	}
	close(open)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
}

// TestGrowingHLSServesWhileCompleteReadsTheTail: Complete read the last segment's tail (peekTail) with the plan's
// lock held, every request waiting on it; the tail is now read first and published after.
func TestGrowingHLSServesWhileCompleteReadsTheTail(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	m := mkv.NewMemFS()
	fs := *m.FS()
	var armed atomic.Bool
	reached, open, once := make(chan struct{}), make(chan struct{}), &sync.Once{}
	gate := &gatedFile{}
	base := fs.Open
	fs.Open = func(path string) (mkv.ReadSeekCloser, error) {
		f, err := base(path)
		if err != nil {
			return nil, err
		}
		return &gatedFile{ReadSeekCloser: f, from: gate.from, to: gate.to, armed: &armed, reached: reached, open: open, once: once}, nil
	}
	fs.OpenContext = nil
	m.Put("in.mkv", data[:clusters[8].end])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: &fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	want, err := plan.Segment(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	gate.from, gate.to = plan.full.offsets[len(plan.full.offsets)-1], clusters[8].end // the tail, not the cursor's header
	armed.Store(true)
	completed := make(chan error, 1)
	go func() { completed <- plan.Complete() }()
	<-reached // Complete is reading the tail

	served := make(chan error, 1)
	go func() {
		got, err := plan.Segment(context.Background(), 0)
		if err == nil && string(got) != string(want) {
			err = io.ErrUnexpectedEOF
		}
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("a request while Complete reads the tail: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a request waited on Complete reading the tail")
	}
	close(open)
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}
