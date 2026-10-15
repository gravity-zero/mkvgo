package mp4

import (
	"bytes"
	"context"
	"testing"
)

// TestSourceReadBytesCountsEveryRead: HLSPlanStats.SourceReadBytes is every
// byte the plan's requests read from the source - windows, a streamed segment,
// an audio rendition - measured against a filesystem that tallies the reads;
// the build's own walks are not counted.
func TestSourceReadBytesCountsEveryRead(t *testing.T) {
	src := buildInterleavedSource(t, 20)
	var tally ioTally
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{FS: tallyFS(&tally), SegmentMs: 2000, StreamFromFS: true})
	if err != nil {
		t.Fatal(err)
	}
	built := tally.read
	if got := plan.Stats().SourceReadBytes; got != 0 {
		t.Errorf("SourceReadBytes = %d before any request, want 0", got)
	}
	for n := 0; n < 3; n++ {
		if _, err := plan.Segment(ctx, n); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	if _, _, err := plan.Resource(ctx, "seg_a1_00005.m4s"); err != nil {
		t.Fatalf("audio segment: %v", err)
	}
	h, err := plan.Open(ctx, plan.SegmentName(6))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var out bytes.Buffer
	if _, err := h.WriteTo(ctx, &out); err != nil {
		t.Fatalf("stream: %v", err)
	}
	got, want := plan.Stats().SourceReadBytes, tally.read-built
	if got != want || got == 0 {
		t.Errorf("SourceReadBytes = %d, the filesystem served %d bytes to the plan's requests", got, want)
	}
	// The default filesystem: the source is an OS file, read through its own file.
	local, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	seg, err := local.Segment(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := local.Stats().SourceReadBytes; got < int64(len(seg))/2 {
		t.Errorf("local SourceReadBytes = %d for a %d-byte segment", got, len(seg))
	}
}
