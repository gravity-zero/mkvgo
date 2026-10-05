package mp4

import (
	"context"
	"testing"
)

// The plan counts what its windows cost: a walk of the source per window
// built, and whether that window had already been built once - the read a
// server pays twice. Video then audio of the same segment is one walk; asking
// for a rendition that was already collected is a second walk of the whole
// window, and what it built beside the one rendition served is dropped.
func TestHLSPlanStatsCountsWindowWalks(t *testing.T) {
	src, _ := zeroTailMKV(t, 0)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Stats(); got != (HLSPlanStats{}) {
		t.Fatalf("a plan nobody asked a segment of reports %+v", got)
	}
	get := func(name string) int64 {
		t.Helper()
		b, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return int64(len(b))
	}

	video := get("seg00001.m4s")
	audio := get("seg_a1_00001.m4s")
	want := HLSPlanStats{WindowBuilds: 1, SharedRenditions: 1, BuiltBytes: video + audio, ServedBytes: video + audio}
	if got := plan.Stats(); got != want {
		t.Errorf("video then audio of one segment:\n got %+v\nwant %+v", got, want)
	}

	// The same video again: its window is gone, so it is walked again, and the
	// audio that walk built is not kept for anyone.
	get("seg00001.m4s")
	want.WindowBuilds, want.WindowRebuilds = 2, 1
	want.BuiltBytes += video + audio
	want.ServedBytes += video
	if got := plan.Stats(); got != want {
		t.Errorf("the same video a second time:\n got %+v\nwant %+v", got, want)
	}
	get("seg_a1_00001.m4s") // served from the window the second video request built
	want.SharedRenditions++
	want.ServedBytes += audio
	if got := plan.Stats(); got != want {
		t.Errorf("then its audio:\n got %+v\nwant %+v", got, want)
	}

	// A window whose audio nobody collects is pushed out by the next ones once
	// the budget is spent: counted as an eviction, its bytes as dropped.
	tight, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, WindowCacheBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seg00001.m4s", "seg00002.m4s", "seg00003.m4s"} {
		if _, _, err := tight.Resource(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	got := tight.Stats()
	if got.WindowBuilds != 3 || got.WindowRebuilds != 0 || got.Evictions < 2 || got.DroppedBytes == 0 {
		t.Errorf("three videos under a one-byte budget: %+v, want 3 builds, no rebuild, at least 2 evictions and dropped bytes", got)
	}
	if got.BuiltBytes != got.ServedBytes+got.DroppedBytes+tight.heldBytes() {
		t.Errorf("built %d != served %d + dropped %d + held %d", got.BuiltBytes, got.ServedBytes, got.DroppedBytes, tight.heldBytes())
	}

	// Sharing off: every request walks, and every byte built is either the
	// rendition served or dropped.
	off, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, WindowCacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seg00001.m4s", "seg_a1_00001.m4s"} {
		if _, _, err := off.Resource(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	got = off.Stats()
	if got.WindowBuilds != 2 || got.WindowRebuilds != 1 || got.SharedRenditions != 0 {
		t.Errorf("sharing off, video then audio: %+v, want 2 builds of which 1 rebuild, nothing shared", got)
	}
	if got.BuiltBytes != got.ServedBytes+got.DroppedBytes || got.ServedBytes != video+audio {
		t.Errorf("sharing off: built %d, served %d (want %d), dropped %d", got.BuiltBytes, got.ServedBytes, video+audio, got.DroppedBytes)
	}
}
