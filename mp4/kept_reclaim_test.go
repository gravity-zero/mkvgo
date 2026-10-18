package mp4

import (
	"context"
	"testing"
)

// TestStaleKeptSegmentsMakeRoom pins that video a walk kept for a viewer who never came for it does not hold the
// process budget: a window walked later reclaims it, oldest first, and keeps its own segments.
func TestStaleKeptSegmentsMakeRoom(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	SetWindowBytes(max(plan.windowSpan(0), plan.windowSpan(1))) // room for one window at a time
	defer SetWindowBytes(0)
	for _, name := range []string{"seg_a1_00001.m4s", "seg_a1_00002.m4s"} { // audio first, video never asked for
		h, err := plan.Open(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		h.Close()
	}
	if st := plan.Stats(); st.KeptFallbacks != 0 {
		t.Errorf("%d windows kept nothing: the first window's uncollected video held the budget", st.KeptFallbacks)
	}
}
