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

// TestKeptWindowServesWithoutRereading: the walk that builds a window's table
// reads the whole window, so its segments are written from what it read - the
// source is not read a second time for the video nor for the audio - and the
// bytes are Resource's. With the process-wide budget full (SetWindowBytes) the
// table is built structure-only and the segments are copied from the source,
// as before, byte for byte the same.
func TestKeptWindowServesWithoutRereading(t *testing.T) {
	src := buildInterleavedSource(t, 20)
	ctx := context.Background()
	var tally ioTally
	opts := Options{FS: tallyFS(&tally), SegmentMs: 2000, StreamFromFS: true, SourceAccess: SourceLocal}
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

	copyFromSource(t)
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
	if st := full.Stats(); st.KeptSegments != 0 || st.KeptFallbacks != 1 {
		t.Errorf("budget full: kept %d segments, %d fallbacks, want 0 and 1", st.KeptSegments, st.KeptFallbacks)
	}
}
