package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A cap of one: the second walk queues until the first releases, gives up
// with its context, and the in-flight count returns to zero.
func TestAcquireWalkCap(t *testing.T) {
	SetMaxConcurrentWalks(1)
	defer SetMaxConcurrentWalks(0)
	release, waited, err := acquireWalk(context.Background())
	if err != nil || waited || WalksInFlight() != 1 {
		t.Fatalf("first slot: err=%v waited=%v inflight=%d", err, waited, WalksInFlight())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, waited, err := acquireWalk(ctx); err == nil || !waited {
		t.Fatalf("second slot must queue then give up with its context: err=%v waited=%v", err, waited)
	}
	release()
	if WalksInFlight() != 0 {
		t.Fatalf("in flight after release: %d", WalksInFlight())
	}
	release2, waited, err := acquireWalk(context.Background())
	if err != nil || waited {
		t.Fatalf("slot after release: err=%v waited=%v", err, waited)
	}
	release2()
	SetMaxConcurrentWalks(0)
	for i := 0; i < 3; i++ { // no cap: nobody queues
		r, waited, err := acquireWalk(context.Background())
		if err != nil || waited {
			t.Fatalf("uncapped slot %d: err=%v waited=%v", i, err, waited)
		}
		defer r()
	}
}

// Under a cap of one, concurrent requests for different windows all serve the
// full pass's bytes, some of them after queueing.
func TestPlanHLSCappedWalks(t *testing.T) {
	SetMaxConcurrentWalks(1)
	defer SetMaxConcurrentWalks(0)
	src := openGOPSource(t, 6, false, true)
	dir, plan := assertPlanMatchesFullPass(t, src, openGOPMs)
	fresh, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan string, plan.NumSegments())
	for n := 0; n < plan.NumSegments(); n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			got, err := fresh.Segment(context.Background(), n)
			if err != nil {
				errs <- err.Error()
				return
			}
			want, _ := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
			if !bytes.Equal(got, want) {
				errs <- plan.SegmentName(n) + " differs from the full pass"
			}
		}(n)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if WalksInFlight() != 0 {
		t.Errorf("walks still in flight: %d", WalksInFlight())
	}
	if st := fresh.Stats(); st.WindowBuilds != int64(plan.NumSegments()) {
		t.Errorf("%d walks for %d windows", st.WindowBuilds, plan.NumSegments())
	}
}

// Once a walk has shown how heavy the audio runs, the arena of the next window
// is sized on the video's share of the span, and still holds it whole.
func TestArenaSizedOnVideoShare(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 150; i++ { // 2 KiB video frames at 25 fps
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: append(iframeTestFrame(i, i%25 == 0), make([]byte, 2048)...)})
	}
	for i := 0; i < 300; i++ { // 1 KiB audio frames every 20 ms: half the bytes
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: append([]byte{0xAA, byte(i)}, make([]byte, 1024)...)})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	assertPlanMatchesFullPass(t, src, 2000)
	fresh, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	cold := cap(fresh.newWindowInPlace(1).buf) - windowHeadroom
	if _, err := fresh.Segment(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	sized := cap(fresh.newWindowInPlace(1).buf) - windowHeadroom
	video, err := fresh.Segment(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if sized < len(video) || sized > len(video)*11/10 || sized*4 > cold*3 {
		t.Errorf("arena for window 1: %d bytes cold, %d once the audio's share is known, video %d", cold, sized, len(video))
	}
	if st := fresh.Stats(); st.ArenaFallbacks != 0 {
		t.Errorf("%d arena fallbacks on constant-rate audio", st.ArenaFallbacks)
	}
}

// Variable-rate audio: a window quieter than any seen falls back to copy once,
// lowers the share taken off, and the windows after it fit again.
func TestArenaVariableAudioFallsBackOnce(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 250; i++ { // 10 s of 2 KiB video frames
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: append(iframeTestFrame(i, i%25 == 0), make([]byte, 2048)...)})
	}
	for i := 0; i < 500; i++ { // loud audio in the first window, half of it afterwards
		size := 1536
		if i*20 >= 2000 {
			size = 768
		}
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: append([]byte{0xAA, byte(i)}, make([]byte, size)...)})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	assertPlanMatchesFullPass(t, src, 2000)
	fresh, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < fresh.NumSegments(); n++ {
		if _, err := fresh.Segment(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	if st := fresh.Stats(); st.ArenaFallbacks != 1 {
		t.Errorf("%d arena fallbacks, want exactly one (the first quiet window)", st.ArenaFallbacks)
	}
}
