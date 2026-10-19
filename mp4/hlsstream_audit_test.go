package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// awaitFlight waits until window n's flight on plan (the table's with table) meets ok, instead of a sleep guessing it.
func awaitFlight(t *testing.T, plan *HLSPlan, table bool, n int, ok func(*flight) bool) {
	t.Helper()
	mu := &plan.winMu
	if table {
		mu = &plan.tabMu
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		mu.Lock()
		var f *flight
		if table {
			if tf := plan.tabFlight[n]; tf != nil {
				f = tf.flight
			}
		} else if wf := plan.winFlight[n]; wf != nil {
			f = wf.flight
		}
		met := f != nil && ok(f)
		mu.Unlock()
		if met {
			return
		}
	}
	t.Fatal("the flight never reached the state the test waits for")
}

func flightStarted(*flight) bool      { return true }
func flightJoined(f *flight) bool     { return f.waiters > 0 }
func flightLeaderGone(f *flight) bool { return f.gone }

// A client that gives up on the window build it started does not fail the
// clients waiting on that build: the build goes on for them.
func TestWindowBuildSurvivesLeaderCancel(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	for _, mode := range []string{"window", "table"} {
		t.Run(mode, func(t *testing.T) {
			plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
			if err != nil {
				t.Fatal(err)
			}
			SetMaxConcurrentWalks(1)
			defer SetMaxConcurrentWalks(0)
			release, _, err := acquireWalk(context.Background()) // the only slot: every build queues
			if err != nil {
				t.Fatal(err)
			}
			build := func(ctx context.Context) error {
				if mode == "table" {
					_, err := plan.table(ctx, 1)
					return err
				}
				_, _, err := plan.window(ctx, 1)
				return err
			}
			leaderCtx, cancel := context.WithCancel(context.Background())
			leader := make(chan error, 1)
			go func() { leader <- build(leaderCtx) }()
			awaitFlight(t, plan, mode == "table", 1, flightStarted) // the leader is queued on the slot
			waiter := make(chan error, 1)
			go func() { waiter <- build(context.Background()) }()
			awaitFlight(t, plan, mode == "table", 1, flightJoined)
			cancel()
			awaitFlight(t, plan, mode == "table", 1, flightLeaderGone)
			release()
			select {
			case err := <-waiter:
				if err != nil {
					t.Fatalf("the waiter failed with the leader's cancellation: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the waiter never returned")
			}
			<-leader
			if err := build(context.Background()); err != nil {
				t.Fatalf("after the race: %v", err)
			}
		})
	}
}

// A build whose every client has gone is cancelled, not run to the end for nobody.
func TestWindowBuildStopsWhenEveryoneLeaves(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	SetMaxConcurrentWalks(1)
	defer SetMaxConcurrentWalks(0)
	release, _, err := acquireWalk(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := plan.window(ctx, 1); done <- err }()
	awaitFlight(t, plan, false, 1, flightStarted)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a build nobody waits on kept queueing")
	}
	plan.winMu.Lock()
	left := len(plan.winFlight)
	plan.winMu.Unlock()
	if left != 0 {
		t.Fatalf("%d flights left behind", left)
	}
}

// The stage packs small pieces; a piece the size of a stage or more is written
// on its own, so the stage never grows past its pooled capacity.
func TestSpanCopierStageStaysBounded(t *testing.T) {
	src := make([]byte, 1<<20)
	for i := range src {
		src[i] = byte(i * 7)
	}
	c := spanCopier{src: bytes.NewReader(src), buf: make([]byte, streamBufBytes)}
	defer c.release()
	var out bytes.Buffer
	var want []byte
	for _, s := range []struct{ off, n int64 }{{0, 1000}, {1000, 200 << 10}, {201 << 10, 3000}, {205 << 10, 40 << 10}} {
		if err := c.emit(&out, []byte{1, 2, 3, 4}); err != nil {
			t.Fatal(err)
		}
		if err := c.copy(&out, s.off, s.n); err != nil {
			t.Fatal(err)
		}
		want = append(want, 1, 2, 3, 4)
		want = append(want, src[s.off:s.off+s.n]...)
	}
	if err := c.flush(&out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) || c.written != int64(len(want)) {
		t.Fatalf("copied %d bytes (written %d), want %d", out.Len(), c.written, len(want))
	}
	if c.stage != nil && cap(*c.stage) > stageBytes+64<<10 {
		t.Fatalf("the stage grew to %d bytes, pooled at %d", cap(*c.stage), stageBytes+64<<10)
	}
}

// The count WriteRange returns is what the writer took, flushed tail included.
func TestWriteRangeCountsFlushedTail(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	h, err := plan.Open(context.Background(), "seg00001.m4s")
	if err != nil {
		t.Fatal(err)
	}
	cw := &takenWriter{}
	for _, r := range []struct{ off, n int64 }{{0, h.Size()}, {0, 10}, {h.Size() - 3, 3}, {100, 5000}} {
		cw.n = 0
		m, err := h.WriteRange(context.Background(), cw, r.off, min(r.n, h.Size()-r.off))
		if err != nil || m != cw.n {
			t.Fatalf("WriteRange(%d,%d) returned %d, the writer took %d, err %v", r.off, r.n, m, cw.n, err)
		}
	}
}

type takenWriter struct{ n int64 }

func (c *takenWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// A writer that takes fewer bytes of the head without an error is a short write.
func TestWriteSegmentFileShortHead(t *testing.T) {
	err := writeSegmentFile(&shortWriter{}, []byte("styp+moof+mdat"), 0, nil)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
	err = writeSegmentFile(&shortWriter{}, []byte("head"), 4, bytes.NewReader([]byte("data")))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err = %v, want io.ErrShortWrite", err)
	}
}

type shortWriter struct{ bytes.Buffer }

func (s *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:len(p)-1]
	}
	return s.Buffer.Write(p)
}

// A plan refuses to read a source whose size or mtime moved since it was
// built: its offsets belong to another file. Playlists, built once, still serve.
func TestPlanRefusesChangedSource(t *testing.T) {
	statEveryRequest(t)
	src := openGOPSource(t, 3, false, true)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(ctx, "seg00001.m4s"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Resource(ctx, "seg_a1_00001.m4s"); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(ctx, "seg00001.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open after a touch: err = %v, want ErrSourceChanged", err)
	}
	if _, err := plan.Open(ctx, "seg00002.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open of a fresh window after a touch: err = %v", err)
	}
	if _, _, err := plan.Resource(ctx, "seg_a1_00001.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Resource after a touch: err = %v, want ErrSourceChanged", err)
	}
	for _, name := range []string{"master.m3u8", "playlist.m3u8", "init.mp4"} {
		if _, _, err := plan.Resource(ctx, name); err != nil {
			t.Errorf("%s after a touch: %v", name, err)
		}
	}
}

// A streamed segment is the same bytes whether the writer takes the file
// itself (sendfile) or only buffered writes, on frames above and below sendfileMin.
func TestWriteRangeParityWithAndWithoutSendfile(t *testing.T) {
	src := bigFrameSource(t, 8)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 4000})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seg00001.m4s", "seg00002.m4s", "seg_a1_00001.m4s"} {
		want, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		h, err := plan.Open(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		plain := &sinkWriter{}
		if m, err := h.WriteTo(ctx, plain); err != nil || m != int64(len(want)) || !bytes.Equal(plain.b, want) {
			t.Fatalf("%s buffered: %d bytes (want %d), equal %v, err %v", name, m, len(want), bytes.Equal(plain.b, want), err)
		}
		var viaFile bytes.Buffer // bytes.Buffer takes the file through ReadFrom
		if m, err := h.WriteTo(ctx, &viaFile); err != nil || m != int64(len(want)) || !bytes.Equal(viaFile.Bytes(), want) {
			t.Fatalf("%s sendfile: %d bytes (want %d), err %v", name, m, len(want), err)
		}
		if h.Bytes() == nil && plain.writes >= len(want)/stageBytes+200 {
			t.Errorf("%s: %d writes for %d bytes", name, plain.writes, len(want))
		}
	}
}

type sinkWriter struct {
	b      []byte
	writes int
}

func (s *sinkWriter) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	s.writes++
	return len(p), nil
}

// A build keeps going while any client waits for it and is cancelled once the
// last one leaves; the flight is gone and a newcomer rebuilds from scratch.
func TestWindowBuildCancelsOnceLastWaiterLeaves(t *testing.T) {
	src := openGOPSource(t, 3, false, true)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	SetMaxConcurrentWalks(1)
	defer SetMaxConcurrentWalks(0)
	release, _, err := acquireWalk(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	leader, waiter := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := plan.window(leaderCtx, 1); leader <- err }()
	awaitFlight(t, plan, false, 1, flightStarted)
	go func() { _, _, err := plan.window(waiterCtx, 1); waiter <- err }()
	awaitFlight(t, plan, false, 1, flightJoined)
	cancelLeader()
	select {
	case err := <-leader:
		t.Fatalf("the build ended (%v) while a waiter was still on it", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancelWaiter()
	for _, ch := range []chan error{leader, waiter} {
		select {
		case err := <-ch:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a build nobody waits on kept queueing")
		}
	}
	plan.winMu.Lock()
	left := len(plan.winFlight)
	plan.winMu.Unlock()
	if left != 0 {
		t.Fatalf("%d flights left behind", left)
	}
	release()
	if _, _, err := plan.window(context.Background(), 1); err != nil {
		t.Fatalf("rebuild after abandon: %v", err)
	}
}

// A file renamed over the source with the same size and mtime is another
// file: the plan refuses it on identity, not only on the stamp.
func TestPlanRefusesReplacedSourceWithSameStamp(t *testing.T) {
	statEveryRequest(t)
	src := openGOPSource(t, 3, false, true)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(ctx, "seg00001.m4s"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	twin := src + ".twin"
	if err := os.WriteFile(twin, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(twin, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(twin, src); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(src); statStamp(again) != statStamp(fi) {
		t.Skip("the filesystem did not keep the stamp")
	}
	if _, err := plan.Open(ctx, "seg00001.m4s"); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Open over the replaced file: err = %v, want ErrSourceChanged", err)
	}
}

// Subtitle resources read the source too: a touched source refuses them, the
// subtitle playlist (built once) still serves.
func TestPlanRefusesChangedSourceForSubtitles(t *testing.T) {
	statEveryRequest(t)
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	for i := 0; i < 150; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	for i := 0; i < 3; i++ {
		blocks = append(blocks, genBlock{track: 3, pts: int64(i) * 2000, key: true, data: []byte(fmt.Sprintf("cue %d", i))})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, blocks)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sub1_00001.vtt", "sub1.vtt"} {
		if _, _, err := plan.Resource(ctx, name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sub1_00002.vtt", "sub1.vtt"} {
		if _, _, err := plan.Resource(ctx, name); !errors.Is(err, ErrSourceChanged) {
			t.Errorf("%s after a touch: err = %v, want ErrSourceChanged", name, err)
		}
	}
	if _, _, err := plan.Resource(ctx, "sub1.m3u8"); err != nil {
		t.Errorf("sub1.m3u8 after a touch: %v", err)
	}
}

// statEveryRequest has the plan stat the source's path on every request, as before the interval.
func statEveryRequest(t *testing.T) {
	old := sourceStatEvery.Load()
	sourceStatEvery.Store(0)
	t.Cleanup(func() { sourceStatEvery.Store(old) })
}
