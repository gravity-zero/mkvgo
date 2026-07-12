package mkv

import "testing"

// TestAvgFrameRate covers the average frame rate derivation (the conventional
// avg_frame_rate): frame count over duration when both are known, 0 otherwise.
func TestAvgFrameRate(t *testing.T) {
	// 240 frames over 10 s = 24 fps.
	tr := Track{FrameCount: 240, DurationMs: 10000}
	if got := tr.AvgFrameRate(); got < 23.99 || got > 24.01 {
		t.Errorf("AvgFrameRate = %g, want ~24", got)
	}
	// No frame count (Matroska head-only) → 0, caller falls back to FrameRate.
	noCount := Track{DurationMs: 10000}
	if got := noCount.AvgFrameRate(); got != 0 {
		t.Errorf("no frame count: AvgFrameRate = %g, want 0", got)
	}
	// No duration → 0 (avoids a divide).
	noDur := Track{FrameCount: 100}
	if got := noDur.AvgFrameRate(); got != 0 {
		t.Errorf("no duration: AvgFrameRate = %g, want 0", got)
	}
}
