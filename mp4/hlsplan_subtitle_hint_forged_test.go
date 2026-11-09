package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A Segment declaring more bytes than its file holds (a forged size, a file cut short) has its subtitle scan read
// ahead no further than the file: the range asked of the disk was the declared one, 2^56 bytes one call at a time.
func TestSubtitleScanReadsAheadNoFurtherThanTheFile(t *testing.T) {
	data, err := os.ReadFile("../internal/testdata/sample_subs.mkv")
	if err != nil {
		t.Fatal(err)
	}
	copy(data[44:52], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE}) // the Segment's size: 2^56-2
	path := filepath.Join(t.TempDir(), "forged.mkv")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	type rng struct{ off, n int64 }
	var will []rng
	defer func(w func(*os.File, int64, int64)) { adviseWillNeed = w }(adviseWillNeed)
	adviseWillNeed = func(_ *os.File, off, n int64) { will = append(will, rng{off, n}) }
	defer func(f func(*os.File) bool) { networkSource = f }(networkSource)
	networkSource = func(*os.File) bool { return false }
	ctx := context.Background()
	plan, err := PlanHLS(ctx, path, Options{SegmentMs: 2000, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.subtitleSegment(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if len(will) == 0 {
		t.Fatal("the subtitle scan read nothing ahead")
	}
	for _, w := range will {
		if w.off+w.n > int64(len(data)) {
			t.Errorf("read ahead [%d, %d) of a file of %d bytes", w.off, w.off+w.n, len(data))
		}
	}
}
