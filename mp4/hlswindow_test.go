package mp4

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// The budget must follow the SOURCE, not a constant: a 1080p window runs ~2 MiB
// and a high-bitrate 2160p one ~22 MiB, so any fixed ceiling is wrong for
// somebody. It is twice the largest window seen (so the one being collected and
// the next both fit), floored for small sources, and an explicit setting always
// wins.
func TestBudgetFollowsTheWindowSize(t *testing.T) {
	p := &HLSPlan{}
	if got := p.budget(); got != minWindowCacheBytes {
		t.Errorf("a plan that has seen nothing budgets %d, want the %d floor", got, minWindowCacheBytes)
	}
	p.winPeak = 40 << 20 // a fat 2160p window
	if got, want := p.budget(), int64(80<<20); got != want {
		t.Errorf("with a %d-byte window the budget is %d, want %d (twice it)", p.winPeak, got, want)
	}
	p.winBudget = 5 << 20
	if got, want := p.budget(), int64(5<<20); got != want {
		t.Errorf("an explicit budget of %d was overridden to %d", want, got)
	}
}

// Why the budget must cover a window: if a bundle is evicted before the player
// has collected its audio, the second request re-walks the window and the whole
// saving evaporates - on exactly the biggest files, which need it most. Forcing a
// budget under one window's size must therefore show the amplification come back,
// while still serving the same bytes (a miss is only ever a cost, never a fault).
func TestABudgetSmallerThanOneWindowLosesTheSaving(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()

	read := func(budget int64) (int64, []byte) {
		var tally readTally
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, WindowCacheBytes: budget, FS: countingFS(&tally)})
		if err != nil {
			t.Fatal(err)
		}
		audio := plan.videoIndex() + 1
		var last []byte
		tally = readTally{}
		for i := 0; i < plan.NumSegments(); i++ {
			if _, err := plan.Segment(ctx, i); err != nil { // a viewer: video...
				t.Fatal(err)
			}
			if last, err = plan.segmentTrack(ctx, audio, i); err != nil { // ...then its audio
				t.Fatal(err)
			}
		}
		return tally.bytes, last
	}

	starved, starvedSeg := read(64 << 10) // far under one window
	derived, derivedSeg := read(0)        // the default: twice the largest window

	if starved <= derived {
		t.Errorf("a starved cache read %d bytes and a source-derived one %d: eviction before collection is not showing up",
			starved, derived)
	}
	if !bytes.Equal(starvedSeg, derivedSeg) {
		t.Error("the segment served after an eviction differs from the cached one")
	}
	t.Logf("budget under one window: %.2f MiB read; derived from the source: %.2f MiB",
		float64(starved)/(1<<20), float64(derived)/(1<<20))
}

// A window is dropped on the CONSUMPTION profile - video plus ONE audio track -
// not on exhaustion. A player never comes for the second language or the
// subtitles, so waiting for them keeps every window's leftovers in the cache;
// on a source with heavy audio those leftovers fill it and evict the windows that
// ARE about to be collected, so the saving decays as a viewer watches on and a
// seek loses it outright. After a viewer has taken what a viewer takes, the plan
// must hold nothing at all.
func TestWindowDroppedOnceAViewerHasWhatItTakes(t *testing.T) {
	src := buildInterleavedSource(t, 60) // video + two audio tracks + subtitles
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.tracks) < 3 {
		t.Fatalf("fixture must carry a second audio track nobody asks for (has %d tracks)", len(plan.tracks))
	}
	video := plan.videoIndex()

	for i := 0; i < plan.NumSegments(); i++ {
		if _, err := plan.segmentTrack(ctx, video, i); err != nil {
			t.Fatal(err)
		}
		if _, err := plan.segmentTrack(ctx, video+1, i); err != nil { // one audio track, never the other
			t.Fatal(err)
		}
	}

	plan.winMu.Lock()
	held, bytes := len(plan.windows), plan.winBytes
	plan.winMu.Unlock()
	if held != 0 || bytes != 0 {
		t.Errorf("after a viewer took its video and its audio, the plan still holds %d window(s) / %d bytes of renditions nobody asked for",
			held, bytes)
	}
}

// A DELIVERED rendition must be freed on the spot, not merely flagged. In the
// field `pending` never reaches zero - a player takes its video and ONE audio
// track and never comes for the other languages - so a bundle waits for the byte
// budget to evict it. If delivery does not free, it sits there still holding the
// video it already served: ~76% of the bundle, dead weight, up to the ceiling.
// That is the difference between a read saving and a heap.
func TestDeliveredRenditionIsFreedNotJustFlagged(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	video := plan.videoIndex()
	audio := video + 1

	// What a real viewer does: video + one audio, never the second language.
	var videoBytes int64
	for i := 0; i < plan.NumSegments(); i++ {
		v, err := plan.segmentTrack(ctx, video, i)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.segmentTrack(ctx, audio, i); err != nil {
			t.Fatal(err)
		}
		videoBytes += int64(len(v))
	}

	plan.winMu.Lock()
	held := plan.winBytes
	var stillHasVideo bool
	for _, b := range plan.windows {
		if b.segs[video] != nil {
			stillHasVideo = true
		}
	}
	plan.winMu.Unlock()

	if stillHasVideo {
		t.Error("a window still holds the video segment it already delivered")
	}
	// Only the uncollected second audio track may remain - a fraction of the
	// video that was served past it.
	if held >= videoBytes/2 {
		t.Errorf("plan holds %d bytes after delivering %d bytes of video: delivered renditions are not being freed",
			held, videoBytes)
	}
}

// The other side of the drop rule: a window whose audio has NOT been collected
// yet must be held - that is the whole point, the audio request is seconds behind
// the video one and must not have to re-walk. Exactly one window, waiting.
func TestWindowAwaitingItsAudioIsHeld(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Segment(ctx, 1); err != nil { // video, and nothing else yet
		t.Fatal(err)
	}
	plan.winMu.Lock()
	held := len(plan.windows)
	plan.winMu.Unlock()
	if held != 1 {
		t.Fatalf("%d windows held after a video-only fetch, want the 1 waiting for its audio", held)
	}

	// And it is served from that window - no second walk.
	var tally readTally
	plan.fs = countingFS(&tally)
	if _, err := plan.segmentTrack(ctx, plan.videoIndex()+1, 1); err != nil {
		t.Fatal(err)
	}
	if tally.bytes != 0 {
		t.Errorf("the audio of a held window read %d source bytes; it was already built", tally.bytes)
	}
}

// The byte budget must actually bound the plan: a client that pulls only the
// video of many windows leaves every bundle half-collected, and the cache has to
// stay under its ceiling rather than grow with the file.
func TestWindowCacheHonoursItsByteBudget(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	const budget = 4 << 20
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, WindowCacheBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < plan.NumSegments(); i++ {
		if _, err := plan.Segment(ctx, i); err != nil { // video only: nothing is ever fully collected
			t.Fatal(err)
		}
	}
	plan.winMu.Lock()
	bytes := plan.winBytes
	plan.winMu.Unlock()
	if bytes > budget {
		t.Errorf("window cache holds %d bytes, over its %d budget", bytes, budget)
	}
}

// Turning the cache off must change nothing but the I/O: the same bytes come
// out, every rendition simply re-walks its window. This is the property that
// keeps a miss safe - and a miss is what a seek, a fresh plan or an evicted
// bundle all are.
func TestWindowCacheOffServesIdenticalBytes(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	ctx := context.Background()
	on, err := PlanHLS(ctx, src, Options{SegmentMs: 6000})
	if err != nil {
		t.Fatal(err)
	}
	off, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, WindowCacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(off.windows) != 0 {
		t.Fatal("the cache is meant to be off")
	}
	for ti := 0; ti < len(on.tracks); ti++ {
		for i := 0; i < on.NumSegments(); i++ {
			want, err := off.segmentTrack(ctx, ti, i)
			if err != nil {
				t.Fatal(err)
			}
			got, err := on.segmentTrack(ctx, ti, i)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("track %d segment %d differs with the window cache on (%d bytes) and off (%d bytes)",
					ti, i, len(got), len(want))
			}
		}
	}
	off.winMu.Lock()
	held := len(off.windows)
	off.winMu.Unlock()
	if held != 0 {
		t.Errorf("cache disabled but %d bundles retained", held)
	}
}

// hls.js pulls a window's video and audio in PARALLEL, and nothing orders them
// on the server. Both requests must be served by ONE walk (the second waits for
// the first rather than opening its own), or the parallel case would read the
// window twice - exactly what this is meant to stop.
func TestParallelRenditionsOfOneWindowShareASingleWalk(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	var tally readTally
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, FS: countingFS(&tally)})
	if err != nil {
		t.Fatal(err)
	}
	tracks := len(plan.tracks)

	// Reference: what ONE walk of this window reads.
	solo, err := PlanHLS(ctx, src, Options{SegmentMs: 6000, WindowCacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	var oneWalk readTally
	solo.fs = countingFS(&oneWalk)
	if _, err := solo.Segment(ctx, 3); err != nil {
		t.Fatal(err)
	}

	tally = readTally{}
	var wg sync.WaitGroup
	got := make([][]byte, tracks)
	for ti := 0; ti < tracks; ti++ {
		wg.Add(1)
		go func(ti int) {
			defer wg.Done()
			data, err := plan.segmentTrack(ctx, ti, 3)
			if err != nil {
				t.Error(err)
				return
			}
			got[ti] = data
		}(ti)
	}
	wg.Wait()

	// Every rendition of the window, fetched at once, must cost no more source
	// reads than the single walk one of them needs on its own.
	if slack := oneWalk.bytes / 10; tally.bytes > oneWalk.bytes+slack {
		t.Errorf("%d renditions fetched in parallel read %d bytes; one walk reads %d - the window was walked more than once",
			tracks, tally.bytes, oneWalk.bytes)
	}
	for ti := 0; ti < tracks; ti++ {
		want, err := solo.segmentTrack(ctx, ti, 3)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[ti], want) {
			t.Errorf("track %d: the segment served through the shared walk differs from an uncached one", ti)
		}
	}
}
