package mp4

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// An indexed subtitle scan over a remote source asks for its blocks, a window each: asked for the whole stride ahead
// (a minute of the file), every seek to the next block asked for the rest of it again and gave up the response.
func TestRemoteIndexedSubtitleScanAsksForItsBlocksOnly(t *testing.T) {
	const src = "../internal/testdata/sample_subs.mkv"
	ctx := context.Background()
	c, err := reader.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	ix := fakeSubIndex{track: 2}
	for {
		b, err := br.Next()
		if err != nil {
			break
		}
		if b.TrackNumber == 2 {
			ix.blocks = append(ix.blocks, reader.IndexedBlock{Pos: br.Pos(), TimeMs: b.Timecode, Frames: 1})
		}
	}
	rs := newRemoteSource(t, src)
	const window = 4 << 10
	fs := httpfs.New(httpfs.Options{WindowSize: window}).Port()
	plan, err := PlanHLS(ctx, rs.srv.URL+"/s.mkv", Options{SegmentMs: 2000, FS: fs, StreamFromFS: true, SubtitleIndex: ix})
	if err != nil {
		t.Fatal(err)
	}
	rs.mu.Lock()
	rs.ranges = nil
	rs.mu.Unlock()
	if _, err := plan.subtitleSegment(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if plan.stats.SubtitleIndexedBlocks == 0 {
		t.Fatal("the index did not serve the scan")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var asked int64
	for _, r := range rs.ranges {
		lo, hi, ok := strings.Cut(strings.TrimPrefix(r, "bytes="), "-")
		a, _ := strconv.ParseInt(lo, 10, 64)
		b, err := strconv.ParseInt(hi, 10, 64)
		if !ok || err != nil {
			t.Fatalf("range %q", r)
		}
		asked += b - a + 1
		if b-a+1 > 2*window {
			t.Errorf("a request of %d bytes (%s) for a subtitle block, window %d", b-a+1, r, window)
		}
	}
	t.Logf("%d requests, %d bytes asked for %d indexed blocks", len(rs.ranges), asked, plan.stats.SubtitleIndexedBlocks)
}
