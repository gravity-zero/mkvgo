package mp4

import (
	"context"
	"fmt"
	"io"
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
	if got := windowStats(plan); got != (HLSPlanStats{}) {
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
	want := HLSPlanStats{WindowBuilds: 1, SharedRenditions: 1, BuiltBytes: video + audio, ServedBytes: video + audio, ServedSegments: 2}
	if got := windowStats(plan); got != want {
		t.Errorf("video then audio of one segment:\n got %+v\nwant %+v", got, want)
	}

	// The same video again: its window is gone, so it is walked again, and the
	// audio that walk built is not kept for anyone.
	get("seg00001.m4s")
	want.WindowBuilds, want.WindowRebuilds = 2, 1
	want.BuiltBytes += video + audio
	want.ServedBytes += video
	want.ServedSegments++
	if got := windowStats(plan); got != want {
		t.Errorf("the same video a second time:\n got %+v\nwant %+v", got, want)
	}
	get("seg_a1_00001.m4s") // served from the window the second video request built
	want.SharedRenditions++
	want.ServedBytes += audio
	want.ServedSegments++
	if got := windowStats(plan); got != want {
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
	got := windowStats(tight)
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
	got = windowStats(off)
	if got.WindowBuilds != 2 || got.WindowRebuilds != 1 || got.SharedRenditions != 0 {
		t.Errorf("sharing off, video then audio: %+v, want 2 builds of which 1 rebuild, nothing shared", got)
	}
	if got.BuiltBytes != got.ServedBytes+got.DroppedBytes || got.ServedBytes != video+audio {
		t.Errorf("sharing off: built %d, served %d (want %d), dropped %d", got.BuiltBytes, got.ServedBytes, video+audio, got.DroppedBytes)
	}
}

// windowStats is the plan's counters without the read counters, whose value depends on the walks' read sizes and
// has its own test (TestSourceReadBytesCountsEveryRead).
func windowStats(p *HLSPlan) HLSPlanStats {
	s := p.Stats()
	s.SourceReadBytes, s.WalkReadBytes, s.CopyReadBytes = 0, 0, 0
	return s
}

// What a plan served is counted on both paths: a segment Resource hands out
// whole is ServedBytes; what Open hands out is StreamedBytes as it is sent,
// streamed or kept (a range writes only its part), and a resource Bytes hands
// out whole, once; each segment is one ServedSegments.
func TestHLSPlanStatsCountsStreamedBytes(t *testing.T) {
	for _, kept := range []bool{true, false} {
		t.Run(fmt.Sprintf("kept=%v", kept), func(t *testing.T) {
			if !kept {
				copyFromSource(t)
			}
			src, _ := zeroTailMKV(t, 0)
			ctx := context.Background()
			plan, err := PlanHLS(ctx, src, keptOptions(kept, Options{SegmentMs: 2000}))
			if err != nil {
				t.Fatal(err)
			}
			open := func(name string) *ResourceHandle {
				t.Helper()
				h, err := plan.Open(ctx, name)
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
			v := open("seg00001.m4s")
			if n, err := v.WriteTo(ctx, io.Discard); err != nil || n != v.Size() {
				t.Fatalf("video: wrote %d of %d: %v", n, v.Size(), err)
			}
			v.Close()
			if st := plan.Stats(); st.StreamedBytes != v.Size() || st.ServedSegments != 1 || st.ServedBytes != 0 || st.StreamFallbacks != 0 || (st.KeptSegments > 0) != kept {
				t.Errorf("one video served whole (%d bytes): %+v", v.Size(), st)
			}
			a := open("seg_a1_00001.m4s")
			half := a.Size() / 2
			if n, err := a.WriteRange(ctx, io.Discard, 0, half); err != nil || n != half {
				t.Fatalf("audio: wrote %d of %d: %v", n, half, err)
			}
			a.Close()
			want := v.Size() + half // streamed or kept: what was written
			if st := plan.Stats(); st.StreamedBytes != want || st.ServedSegments != 2 {
				t.Errorf("then half an audio (%d of %d bytes): %+v, want %d streamed", half, a.Size(), st, want)
			}
			data, _, err := plan.Resource(ctx, "seg00002.m4s")
			if err != nil {
				t.Fatal(err)
			}
			if st := plan.Stats(); st.StreamedBytes != want || st.ServedBytes != int64(len(data)) || st.ServedSegments != 3 {
				t.Errorf("then a video built whole by Resource (%d bytes): %+v", len(data), st)
			}
			// A handle opened and closed unwritten sent nothing.
			idle := open("init.mp4")
			if !idle.Whole() {
				t.Fatal("init.mp4: not held whole")
			}
			idle.Close()
			if st := plan.Stats(); st.StreamedBytes != want {
				t.Errorf("then an init opened and closed unwritten: %d streamed, want %d", st.StreamedBytes, want)
			}
			// Bytes hands it out whole, counted once: a server writes it itself, and a write after counts nothing more.
			in := open("init.mp4")
			first, again := in.Bytes(), in.Bytes()
			if first == nil || again == nil {
				t.Fatal("init.mp4: not handed out whole")
			}
			if _, err := in.WriteTo(ctx, io.Discard); err != nil {
				t.Fatal(err)
			}
			if st := plan.Stats(); st.StreamedBytes != want+in.Size() || st.ServedSegments != 3 {
				t.Errorf("then an init taken by Bytes twice and written (%d bytes): %+v", in.Size(), st)
			}
			// A range of a whole resource counts its part.
			part := open("init.mp4")
			if n, err := part.WriteRange(ctx, io.Discard, 10, 20); err != nil || n != 20 {
				t.Fatalf("init range: wrote %d: %v", n, err)
			}
			if st := plan.Stats(); st.StreamedBytes != want+in.Size()+20 {
				t.Errorf("then 20 bytes of an init: %d streamed, want %d", st.StreamedBytes, want+in.Size()+20)
			}
		})
	}
}

// TestHLSPlanStatsCountsMP4SourceSegments: a segment cut from an MP4's sample table and handed out whole is built and
// served like a Matroska window's, so a server's read-to-served ratio has bytes on both sides.
func TestHLSPlanStatsCountsMP4SourceSegments(t *testing.T) {
	ctx := context.Background()
	plan, err := PlanHLS(ctx, hlsplanMP4Source(t), Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, name := range []string{"seg00001.m4s", "seg_a1_00001.m4s"} {
		b, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		total += int64(len(b))
	}
	if st := plan.Stats(); st.ServedBytes != total || st.BuiltBytes != total || st.ServedSegments != 2 {
		t.Errorf("two MP4-source segments of %d bytes: %+v", total, st)
	}
}
