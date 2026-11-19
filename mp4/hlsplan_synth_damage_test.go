package mp4

import (
	"bytes"
	"context"
	"testing"
)

// A source without Cues and with a damaged region inside gets its index synthesized past the region, as its window
// walks pass over it: the synthesis stopped at the first damaged region, and the last window then ran over the rest
// of the file (a real 43 min episode planned 35 windows, the last served as one 2.2 GB segment).
func TestPlanHLSSynthesizeIndexPastADamagedRegion(t *testing.T) {
	var blocks []genBlock
	frame := make([]byte, 4<<10)
	frame[0], frame[1], frame[2], frame[3], frame[4] = 0, 0, 0, 1, 0x65
	for i := 0; i < 1500; i++ { // 60 s of video, 40 ms frames, a keyframe each second
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: frame})
	}
	tracks := synthFixtureTracks()[:1]
	clean := buildNoCueMKV(t, tracks, blocks)
	offs := clusterOffsets(t, clean)
	if len(offs) < 50 {
		t.Fatalf("%d clusters, want one a second", len(offs))
	}
	bad := zeroedSource(t, clean, offs[30]+20<<10, offs[30]+40<<10) // 20 KiB inside the 30 s cluster
	ctx := context.Background()
	want, err := PlanHLS(ctx, clean, Options{SegmentMs: 2000, SynthesizeIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, bad, Options{SegmentMs: 2000, SynthesizeIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() != want.NumSegments() {
		t.Fatalf("%d windows planned, %d on the undamaged file", plan.NumSegments(), want.NumSegments())
	}
	last := plan.NumSegments() - 1
	got, err := plan.Segment(ctx, last)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := want.Segment(ctx, last)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ref) {
		t.Errorf("the last window differs from the undamaged file's (%d vs %d bytes)", len(got), len(ref))
	}
}
