package mp4

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// copyFromSource makes every window of the test copy its segments from the source, as over the window budget.
func copyFromSource(t *testing.T) {
	t.Helper()
	SetWindowBytes(-1)
	t.Cleanup(func() { SetWindowBytes(0) })
}

// keptOptions is o with the source taken for a network one when kept: only a network source's walks keep segments.
func keptOptions(kept bool, o Options) Options {
	if kept {
		o.SourceAccess = SourceNetwork
	}
	return o
}

// serveWindow opens and writes window n's video and first audio segments the way a viewer takes them.
func serveWindow(t *testing.T, p *HLSPlan, n int) (video, audio []byte) {
	t.Helper()
	ctx := context.Background()
	var out [2]bytes.Buffer
	for i, name := range []string{p.SegmentName(n), fmt.Sprintf("seg_a1_%05d.m4s", n+1)} {
		h, err := p.Open(ctx, name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if _, err := h.WriteTo(ctx, &out[i]); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		h.Close()
	}
	return out[0].Bytes(), out[1].Bytes()
}

// TestKeptWindowServesWithoutRereading: the walk that builds a network
// source's window table reads the whole window, so its segments are written from what it read - the
// source is not read a second time for the video nor for the audio - and the
// bytes are Resource's. With the process-wide budget full (SetWindowBytes) the
// table is built structure-only and the segments are copied from the source,
// as before, byte for byte the same.
func TestKeptWindowServesWithoutRereading(t *testing.T) {
	src := buildInterleavedSource(t, 20)
	ctx := context.Background()
	var tally ioTally
	opts := Options{FS: tallyFS(&tally), SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceNetwork}
	plan, err := PlanHLS(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n < 4; n++ {
		before := plan.Stats().SourceReadBytes
		if _, err := plan.table(ctx, n); err != nil {
			t.Fatal(err)
		}
		walked := plan.Stats().SourceReadBytes
		if walked == before {
			t.Fatalf("window %d: the table's walk read nothing", n)
		}
		video, audio := serveWindow(t, plan, n)
		if got := plan.Stats().SourceReadBytes; got != walked {
			t.Errorf("window %d: serving read %d bytes of the source after the walk, want 0", n, got-walked)
		}
		wantV, _, _ := ref.Resource(ctx, ref.SegmentName(n))
		wantA, _, _ := ref.Resource(ctx, fmt.Sprintf("seg_a1_%05d.m4s", n+1))
		if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
			t.Errorf("window %d: kept segments differ from Resource's", n)
		}
	}
	if st := plan.Stats(); st.KeptSegments != 6 || st.KeptFallbacks != 0 {
		t.Errorf("kept %d segments, %d fallbacks, want 6 and 0", st.KeptSegments, st.KeptFallbacks)
	}
	if held := windowBytesTotal.Load(); held != 0 {
		t.Errorf("%d window bytes still held after every handle was closed", held)
	}
	// A second viewer on a window whose video was taken: copied from the source, the same bytes.
	video, _ := serveWindow(t, plan, 2)
	if wantV, _, _ := ref.Resource(ctx, ref.SegmentName(2)); !bytes.Equal(video, wantV) {
		t.Error("window 2 served again differs from Resource's")
	}

	SetWindowBytes(1) // a budget no window fits
	defer SetWindowBytes(0)
	full, err := PlanHLS(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := full.table(ctx, 5); err != nil {
		t.Fatal(err)
	}
	walked := full.Stats().SourceReadBytes
	video, audio := serveWindow(t, full, 5)
	if got := full.Stats().SourceReadBytes; got == walked {
		t.Error("budget full: the segments were not copied from the source")
	}
	wantV, _, _ := ref.Resource(ctx, ref.SegmentName(5))
	wantA, _, _ := ref.Resource(ctx, "seg_a1_00006.m4s")
	if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
		t.Error("budget full: copied segments differ from Resource's")
	}
	if st := full.Stats(); st.KeptSegments != 1 || st.KeptFallbacks != 1 { // the light audio rides in a network source's table
		t.Errorf("budget full: kept %d segments, %d fallbacks, want 1 (the audio) and 1", st.KeptSegments, st.KeptFallbacks)
	}
}

// A local file's walk keeps nothing, budget or not: its segments are copied from
// the page cache the walk just filled, the bytes Resource's (keeping them cost a
// peak of twice the budget for no read saved, measured).
func TestLocalWindowKeepsNothing(t *testing.T) {
	src := buildInterleavedSource(t, 20)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n < 4; n++ {
		video, audio := serveWindow(t, plan, n)
		wantV, _, _ := ref.Resource(ctx, ref.SegmentName(n))
		wantA, _, _ := ref.Resource(ctx, fmt.Sprintf("seg_a1_%05d.m4s", n+1))
		if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
			t.Errorf("window %d: copied segments differ from Resource's", n)
		}
	}
	if st := plan.Stats(); st.KeptSegments != 0 || st.StreamedSegments != 6 {
		t.Errorf("kept %d, streamed %d, want 0 and 6", st.KeptSegments, st.StreamedSegments)
	}
	if held := windowBytesTotal.Load(); held != 0 {
		t.Errorf("%d window bytes held by a local source", held)
	}
}

// KeptFallbacks counts the windows the budget refused, nothing else: a local
// source never keeps by policy, and a keep turned off refuses nothing.
func TestKeptFallbacksCountsRefusalsOnly(t *testing.T) {
	src := buildInterleavedSource(t, 20)
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		access SourceAccess
		budget int64
		want   int64
	}{
		{"local", SourceLocal, 0, 0},
		{"network, keep off", SourceNetwork, -1, 0},
		{"network, budget full", SourceNetwork, 1, 3},
		{"network, room", SourceNetwork, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			SetWindowBytes(c.budget)
			defer SetWindowBytes(0)
			plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, SourceAccess: c.access})
			if err != nil {
				t.Fatal(err)
			}
			for n := 1; n < 4; n++ {
				serveWindow(t, plan, n)
			}
			if got := plan.Stats().KeptFallbacks; got != c.want {
				t.Errorf("KeptFallbacks = %d, want %d", got, c.want)
			}
		})
	}
}
