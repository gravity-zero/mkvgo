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
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, SourceAccess: SourceNetwork})
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

// TestDuplicateTableGivesBackItsKeptBytes pins that a window built twice (a cancelled flight's walk finishing beside
// its retry's) returns what the copy not stored had kept to the process budget.
func TestDuplicateTableGivesBackItsKeptBytes(t *testing.T) {
	src := buildInterleavedSource(t, 12)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	table := func() *windowTable {
		windowBytesTotal.Add(100) // what the walk reserved and kept
		return &windowTable{held: 100, kept: make([]int64, len(plan.tracks)), taken: make([]bool, len(plan.tracks)),
			data: make([][]byte, len(plan.tracks)), packed: make([][]byte, len(plan.tracks)), heads: make([][]byte, len(plan.tracks))}
	}
	before := windowBytesTotal.Load()
	first, second := table(), table()
	first.kept[0], second.kept[0] = 100, 100
	plan.tabMu.Lock()
	plan.storeTable(0, first)
	plan.storeTable(0, second)
	plan.tabMu.Unlock()
	if got := windowBytesTotal.Load() - before; got != 100 {
		t.Errorf("the budget holds %d bytes for one stored table of 100", got)
	}
	if d, h := plan.takeKept(second, 0, 0); d != nil || h != nil {
		t.Error("the copy not stored still hands out a kept segment, whose budget is already given back")
	}
	plan.tabMu.Lock()
	plan.dropKept(first)
	plan.tabMu.Unlock()
}
