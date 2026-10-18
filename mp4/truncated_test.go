package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestTruncatedMP4KeepsWhatArrived pins that a faststart MP4 cut short (a download that stopped) remuxes and
// serves what it holds, as players play it up to the cut, instead of being refused whole.
func TestTruncatedMP4KeepsWhatArrived(t *testing.T) {
	ctx := context.Background()
	src := editShiftFixture(t, true)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.mp4")
	if err := os.WriteFile(cut, data[:len(data)*9/10], 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(ctx, cut, out); err != nil {
		t.Fatalf("a cut MP4 was refused: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for {
		if _, err := br.Next(); err != nil {
			break
		}
		blocks++
	}
	if blocks == 0 {
		t.Error("the remux of a cut MP4 holds no block")
	}
	plan, err := PlanHLS(ctx, cut, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatalf("HLS refused a cut MP4: %v", err)
	}
	if _, err := plan.Segment(ctx, 0); err != nil {
		t.Errorf("first segment of a cut MP4: %v", err)
	}
}
