package mp4

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/mkv"
)

// twoAudioMP4 is an MP4 of 40 KiB video frames and two audio renditions stored between them.
func twoAudioMP4(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		frame := bytes.Repeat([]byte{byte(i)}, 40<<10)
		copy(frame, []byte{0, 0, 0, 1, 0x65})
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: frame})
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: bytes.Repeat([]byte{0xAA, byte(i)}, 100)})
		blocks = append(blocks, genBlock{track: 3, pts: int64(i) * 20, key: true, data: bytes.Repeat([]byte{0xBB, byte(i)}, 120)})
	}
	sortGenBlocks(blocks)
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	out := filepath.Join(t.TempDir(), "two.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, out, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	return out
}

// shareFixture is a remote plan over src, Resource's bytes for reference, and the origin's request counter.
func shareFixture(t *testing.T) (plan, ref *HLSPlan, rs *remoteSource) {
	t.Helper()
	src := twoAudioMP4(t)
	ctx := context.Background()
	var err error
	if ref, err = PlanHLS(ctx, src, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	rs = newRemoteSource(t, src)
	if plan, err = PlanHLS(ctx, rs.srv.URL+"/s.mp4", Options{SegmentMs: 2000, FS: httpfs.New().Port(), StreamFromFS: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plan.share.dropAll)
	return plan, ref, rs
}

// serveTo writes resource name of plan to w, checking it against ref's bytes.
func serveTo(t *testing.T, plan, ref *HLSPlan, name string, w io.Writer) {
	t.Helper()
	ctx := context.Background()
	var got bytes.Buffer
	h, err := plan.Open(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteTo(ctx, io.MultiWriter(&got, w)); err != nil {
		t.Fatal(err)
	}
	h.Close()
	want, _, err := ref.Resource(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("%s differs from Resource's", name)
	}
}

func (r *remoteSource) getCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets
}

// A video segment's read keeps its window's audio, every rendition: both audio
// segments of the window are then served with no request of their own, and the
// window goes once both were.
func TestShareVideoThenAudio(t *testing.T) {
	plan, ref, rs := shareFixture(t)
	serveTo(t, plan, ref, "seg00002.m4s", io.Discard)
	gets := rs.getCount()
	serveTo(t, plan, ref, "seg_a1_00002.m4s", io.Discard)
	if windowBytesTotal.Load() == 0 {
		t.Error("the window went before its second audio rendition was served")
	}
	serveTo(t, plan, ref, "seg_a2_00002.m4s", io.Discard)
	if held := windowBytesTotal.Load(); held != 0 {
		t.Errorf("%d bytes held once every rendition was served", held)
	}
	if n := rs.getCount() - gets; n != 0 {
		t.Errorf("the window's audio took %d requests after its video", n)
	}
	if st := plan.Stats(); st.KeptSegments != 2 {
		t.Errorf("KeptSegments = %d, want 2", st.KeptSegments)
	}
}

// slowWriter blocks its first write until released.
type slowWriter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *slowWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered); <-s.release })
	return len(p), nil
}

// An audio segment asked while its window's video is being read waits for that
// read rather than starting its own.
func TestShareAudioWaitsForTheRead(t *testing.T) {
	plan, ref, rs := shareFixture(t)
	slow := &slowWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() { serveTo(t, plan, ref, "seg00002.m4s", slow); close(done) }()
	<-slow.entered
	gets := rs.getCount()
	audio := make(chan struct{})
	go func() { serveTo(t, plan, ref, "seg_a1_00002.m4s", io.Discard); close(audio) }()
	time.Sleep(50 * time.Millisecond) // the audio is waiting on the read
	close(slow.release)
	<-done
	<-audio
	if n := rs.getCount() - gets; n != 0 {
		t.Errorf("the audio asked during the video read took %d requests", n)
	}
}

// An audio segment with no video read of its window, or after its window's
// audio expired, is read the usual way, with the same bytes.
func TestShareFallsBack(t *testing.T) {
	plan, ref, rs := shareFixture(t)
	gets := rs.getCount()
	serveTo(t, plan, ref, "seg_a1_00001.m4s", io.Discard)
	if rs.getCount() == gets {
		t.Error("an audio segment with no video read was served without a request")
	}
	defer func(d time.Duration) { shareTTL = d }(shareTTL)
	shareTTL = 20 * time.Millisecond
	serveTo(t, plan, ref, "seg00002.m4s", io.Discard)
	time.Sleep(60 * time.Millisecond)
	if held := windowBytesTotal.Load(); held != 0 { // given back with no further request to the plan
		t.Errorf("%d window bytes held after the window expired", held)
	}
	gets = rs.getCount()
	serveTo(t, plan, ref, "seg_a1_00002.m4s", io.Discard)
	if rs.getCount() == gets {
		t.Error("an expired window's audio was served without a request")
	}
}

// A window the budget refuses is read as before and counted in KeptFallbacks.
func TestShareBudgetRefused(t *testing.T) {
	SetWindowBytes(1)
	defer SetWindowBytes(0)
	plan, ref, rs := shareFixture(t)
	serveTo(t, plan, ref, "seg00002.m4s", io.Discard)
	gets := rs.getCount()
	serveTo(t, plan, ref, "seg_a1_00002.m4s", io.Discard)
	if rs.getCount() == gets {
		t.Error("the audio of a window the budget refused was served without a request")
	}
	if st := plan.Stats(); st.KeptFallbacks != 1 || st.KeptSegments != 0 {
		t.Errorf("KeptFallbacks %d, KeptSegments %d, want 1 and 0", st.KeptFallbacks, st.KeptSegments)
	}
}

// A local MP4 shares nothing, and its audio waits for no grace: its copies come from the page cache.
func TestShareLocalNone(t *testing.T) {
	defer func(d time.Duration) { shareGrace = d }(shareGrace)
	shareGrace = 2 * time.Second
	src := twoAudioMP4(t)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	serveTo(t, plan, plan, "seg00002.m4s", io.Discard)
	start := time.Now()
	serveTo(t, plan, plan, "seg_a1_00001.m4s", io.Discard) // no video read of its window: a network source would wait the grace
	if d := time.Since(start); d > time.Second {
		t.Errorf("a local audio segment took %s: it waited a grace", d)
	}
	serveTo(t, plan, plan, "seg_a1_00002.m4s", io.Discard)
	if st := plan.Stats(); st.KeptSegments != 0 || windowBytesTotal.Load() != 0 {
		t.Errorf("a local source kept %d segments, %d bytes", st.KeptSegments, windowBytesTotal.Load())
	}
}

// A window whose audio lies away from its video (stored in its own region of
// the file) is not read along: the span would be hundreds of times the video's.
func TestShareReach(t *testing.T) {
	video := []span{{off: 1000, n: 400 << 10}, {off: 1000 + 420<<10, n: 400 << 10}}
	near := []sharedRun{{off: 1000 + 400<<10, data: make([]byte, 0, 20<<10)}}
	far := []sharedRun{{off: 500 << 20, data: make([]byte, 0, 20<<10)}}
	if _, _, ok := withinReach(video, near); !ok {
		t.Error("audio between the frames was judged away from them")
	}
	if _, _, ok := withinReach(video, far); ok {
		t.Error("audio 500 MiB away was read along with the video")
	}
}

// A window given back to the budget lets its bytes go and stops its timer:
// what the budget counts is what the process holds.
func TestShareDropReleases(t *testing.T) {
	var s mp4Share
	w, ok := s.begin(7)
	if !ok {
		t.Fatal("begin refused")
	}
	windowBytesTotal.Add(100)
	s.end(7, w, []sharedRun{{off: 0, data: make([]byte, 100)}}, 100)
	if !s.reclaim(windowBytesBudget.Load()) {
		t.Fatal("nothing reclaimed")
	}
	if w.runs != nil || w.timer.Stop() {
		t.Error("a dropped window kept its bytes or its timer")
	}
	if held := windowBytesTotal.Load(); held != 0 {
		t.Errorf("%d bytes still counted", held)
	}
}
