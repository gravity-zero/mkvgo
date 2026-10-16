package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestWindowWalkStopsWhenATrackEnded: a window past the end of one audio track
// must not walk the rest of the file. The track's cues say where it ends (two
// cue gaps behind the window); before the cues can tell, the one walk that
// meets the end of the source teaches the plan, so no later window pays again.
func TestWindowWalkStopsWhenATrackEnded(t *testing.T) {
	src := buildInterleavedSourceUntil(t, 60, 14_000) // the second audio ends at 14 s of 60
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var tally ioTally
	plan, err := PlanHLS(ctx, src, Options{FS: tallyFS(&tally), SegmentMs: 6000, StreamFromFS: true, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	// Window 2 (12-18 s) holds the track's end: its walk meets the end of the
	// source, once. Every window after is bounded: 3 and 4 by what that walk
	// learnt, 5 and 6 by the cues (two gaps behind).
	for _, n := range []int{2, 3, 4, 5, 6} {
		before := plan.Stats().SourceReadBytes
		if _, err := plan.table(ctx, n); err != nil {
			t.Fatal(err)
		}
		read := plan.Stats().SourceReadBytes - before
		span := plan.windowSpan(n)
		t.Logf("window %d: walk read %d bytes, window span %d, file %d", n, read, span, st.Size())
		if n != 2 && read > 3*span {
			t.Errorf("window %d: the walk read %d bytes for a window of %d (file %d): it walked past the window", n, read, span, st.Size())
		}
		video, audio := serveWindow(t, plan, n)
		wantV, _, _ := ref.Resource(ctx, ref.SegmentName(n))
		wantA, _, _ := ref.Resource(ctx, fmt.Sprintf("seg_a1_%05d.m4s", n+1))
		if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
			t.Errorf("window %d: segments differ from Resource's", n)
		}
	}
	// The ended track's own rendition past its end: a segment without samples, not an error, the same as Resource's.
	name := fmt.Sprintf("seg_a2_%05d.m4s", 6)
	h, err := plan.Open(ctx, name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	var got bytes.Buffer
	if _, err := h.WriteTo(ctx, &got); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	h.Close()
	want, _, err := ref.Resource(ctx, name)
	if err != nil {
		t.Fatalf("resource %s: %v", name, err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("%s: streamed %d bytes, Resource %d", name, got.Len(), len(want))
	}
}

// TestBlockOrderedTailWalksOnce: a source interleaved up to 20 s then written
// track by track (the video's clusters, then the audio's), its audio uncued.
// No window traverses the video to find its audio: the walk looks for a track
// missing past the window in the uncued tail, and every segment is Resource's.
func TestBlockOrderedTailWalksOnce(t *testing.T) {
	for _, c := range []struct {
		from, to int64
		noKey    bool
	}{{20_000, 0, false}, {24_000, 0, false}, {20_000, 52_000, false}, {20_000, 50_000, true}} { // inside window 3; on window 4's boundary; interleaved again from inside window 8; the same with that first cluster uncued
		t.Run(fmt.Sprintf("tail%d-%d-%v", c.from, c.to, c.noKey), func(t *testing.T) { testBlockOrderedTail(t, c.from, c.to, c.noKey) })
	}
}

func testBlockOrderedTail(t *testing.T, tailFromMs, tailToMs int64, endNoKey bool) {
	src := buildSourceWithLayout(t, 60, sourceLayout{holeFromMs: -1, holeToMs: -1, tailFromMs: tailFromMs, tailToMs: tailToMs, endNoKey: endNoKey})
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var tally ioTally
	plan, err := PlanHLS(ctx, src, Options{FS: tallyFS(&tally), SegmentMs: 6000, StreamFromFS: true, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	full := t.TempDir() // the full pass, one sequential read: what every segment must equal
	if err := RemuxToHLS(ctx, src, full, Options{SegmentMs: 6000}); err != nil {
		t.Fatal(err)
	}
	var total int64
	for n := 0; n < plan.NumSegments(); n++ {
		before := plan.Stats().SourceReadBytes
		if _, err := plan.table(ctx, n); err != nil {
			t.Fatal(err)
		}
		read := plan.Stats().SourceReadBytes - before
		total += read
		span := plan.windowSpan(n)
		t.Logf("window %d: walk read %d bytes, window span %d, file %d", n, read, span, st.Size())
		if int64(n)*6000+6000 > tailFromMs && read > 4*span {
			t.Errorf("window %d: the walk read %d bytes for a window of %d (file %d): it traversed the tail", n, read, span, st.Size())
		}
		video, audio := serveWindow(t, plan, n)
		wantV, _ := os.ReadFile(filepath.Join(full, plan.SegmentName(n)))
		wantA, _ := os.ReadFile(filepath.Join(full, fmt.Sprintf("seg_a1_%05d.m4s", n+1)))
		if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
			t.Errorf("window %d: segments differ from the full pass's (video %d vs %d bytes, audio %d vs %d)", n, len(video), len(wantV), len(audio), len(wantA))
		}
	}
	if total > 2*st.Size() {
		t.Errorf("serving 8 windows read %d bytes of a %d-byte source", total, st.Size())
	}
}

// TestBlockOrderedTailThenInterleavedEnd: a long block-ordered stretch (its
// audio zone far wider than a window) followed by an interleaved end whose
// first cluster is uncued, with windows that straddle both changes: every
// segment equals the full pass's, so no sample of the end is lost.
func TestBlockOrderedTailThenInterleavedEnd(t *testing.T) {
	src := buildSourceWithLayout(t, 120, sourceLayout{holeFromMs: -1, holeToMs: -1, tailFromMs: 10_000, tailToMs: 100_000, endNoKey: true, videoDiv: 4, audioMul: 4})
	ctx := context.Background()
	const segMs = 3_000 // boundaries at 4 s steps: window 25 runs 100-104 s, the uncued cluster then a cued one
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: segMs, StreamFromFS: true, SourceAccess: SourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	full := t.TempDir()
	if err := RemuxToHLS(ctx, src, full, Options{SegmentMs: segMs}); err != nil {
		t.Fatal(err)
	}
	for n := 22; n < plan.NumSegments(); n++ {
		before := plan.Stats().SourceReadBytes
		video, audio := serveWindow(t, plan, n)
		read := plan.Stats().SourceReadBytes - before
		wantV, _ := os.ReadFile(filepath.Join(full, plan.SegmentName(n)))
		wantA, _ := os.ReadFile(filepath.Join(full, fmt.Sprintf("seg_a1_%05d.m4s", n+1)))
		t.Logf("window %d: read %d, video %d (full pass %d), audio %d (%d)", n, read, len(video), len(wantV), len(audio), len(wantA))
		if !bytes.Equal(video, wantV) || !bytes.Equal(audio, wantA) {
			t.Errorf("window %d: segments differ from the full pass's", n)
		}
	}
	if st := plan.Stats(); st.FarLookups == 0 {
		t.Errorf("no far lookup: the layout did not exercise the uncued regions")
	}
}
