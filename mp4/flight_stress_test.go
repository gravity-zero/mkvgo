package mp4

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestFarLookupsOnAFreshPlanAtOnce: the first clients of a plan just built ask for a window's table and for a track's
// segment at once, every window in turn; each walk looks for the audio in the uncued regions, reading and recording
// where each region opens as it goes, and both clients get the full pass's bytes (the walks share the regions
// through the plan: under -race, a read of one outside the lock is a failure).
func TestFarLookupsOnAFreshPlanAtOnce(t *testing.T) {
	const windows, rounds = 8, 4
	src := openGOPSource(t, windows, false, true)
	ctx := context.Background()
	ref, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	for n := 1; n <= windows; n++ {
		for _, name := range []string{fmt.Sprintf("seg%05d.m4s", n), fmt.Sprintf("seg_a1_%05d.m4s", n)} {
			if want[name], _, err = ref.Resource(ctx, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	for round := 0; round < rounds; round++ {
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
		if err != nil {
			t.Fatal(err)
		}
		for n := 1; n <= windows; n++ {
			video, audio := fmt.Sprintf("seg%05d.m4s", n), fmt.Sprintf("seg_a1_%05d.m4s", n)
			var wg sync.WaitGroup
			var got [2][]byte
			var errs [2]error
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				h, err := plan.Open(ctx, video)
				if err == nil {
					var out bytes.Buffer
					_, err = h.WriteTo(ctx, &out)
					got[0] = out.Bytes()
				}
				errs[0] = err
			}()
			go func() {
				defer wg.Done()
				<-start
				got[1], _, errs[1] = plan.Resource(ctx, audio)
			}()
			close(start)
			wg.Wait()
			for i, name := range []string{video, audio} {
				if errs[i] != nil || !bytes.Equal(got[i], want[name]) {
					t.Fatalf("round %d %s: err %v, %d bytes (want %d)", round, name, errs[i], len(got[i]), len(want[name]))
				}
			}
		}
		if st := plan.Stats(); st.FarLookups == 0 {
			t.Fatalf("round %d: no far lookup, the layout did not exercise the uncued regions", round)
		}
	}
}

// Fifty clients cancelled at random while the builds they share queue and
// run: every client left gets its exact bytes, a cancelled one gets them or
// context.Canceled, no flight or walk slot is left behind, every goroutine
// exits, and every window rebuilds afterwards.
func TestSharedBuildsSurviveRandomCancellations(t *testing.T) {
	const windows, rounds, perRound = 8, 5, 20 // half of each round's clients are cancelled: 50 in all
	base := runtime.NumGoroutine()             // before anything builds: a leak in the reference builds counts too
	src := openGOPSource(t, windows, false, true)
	ctx := context.Background()
	ref, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, 2*windows)
	want := map[string][]byte{}
	for n := 1; n <= windows; n++ {
		for _, name := range []string{fmt.Sprintf("seg%05d.m4s", n), fmt.Sprintf("seg_a1_%05d.m4s", n)} {
			b, _, err := ref.Resource(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			names, want[name] = append(names, name), b
		}
	}
	SetMaxConcurrentWalks(2)
	defer SetMaxConcurrentWalks(0)
	rng := rand.New(rand.NewSource(11))
	var cancelled, cancelledErr, served int
	for round := 0; round < rounds; round++ {
		plan, err := PlanHLS(ctx, src, Options{SegmentMs: openGOPMs})
		if err != nil {
			t.Fatal(err)
		}
		var held []func() // every slot taken: the round's builds queue, their clients wait
		for i := 0; i < 2; i++ {
			r, _, err := acquireWalk(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, r)
		}
		type result struct {
			name   string
			cancel bool
			data   []byte
			err    error
		}
		results := make(chan result, perRound)
		var wg sync.WaitGroup
		for c := 0; c < perRound; c++ {
			name := names[rng.Intn(len(names))]
			stream := rng.Intn(2) == 0
			cctx, cancel := context.WithCancel(ctx)
			doCancel := c%2 == 0
			if doCancel {
				time.AfterFunc(time.Duration(rng.Intn(20))*time.Millisecond, cancel)
			} else {
				defer cancel()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				var data []byte
				var err error
				if stream {
					var h *ResourceHandle
					if h, err = plan.Open(cctx, name); err == nil {
						var out bytes.Buffer
						_, err = h.WriteTo(cctx, &out)
						data = out.Bytes()
					}
				} else {
					data, _, err = plan.Resource(cctx, name)
				}
				results <- result{name, doCancel, data, err}
			}()
		}
		time.Sleep(time.Duration(5+rng.Intn(10)) * time.Millisecond)
		for _, r := range held {
			r()
		}
		wg.Wait()
		close(results)
		for r := range results {
			switch {
			case r.err == nil && bytes.Equal(r.data, want[r.name]):
				served++
			case r.cancel && errors.Is(r.err, context.Canceled):
				cancelledErr++
			default:
				t.Errorf("round %d %s (cancelled %v): err %v, %d bytes (want %d)", round, r.name, r.cancel, r.err, len(r.data), len(want[r.name]))
			}
			if r.cancel {
				cancelled++
			}
		}
		plan.winMu.Lock()
		wf := len(plan.winFlight)
		plan.winMu.Unlock()
		plan.tabMu.Lock()
		tf := len(plan.tabFlight)
		plan.tabMu.Unlock()
		if wf != 0 || tf != 0 || WalksInFlight() != 0 {
			t.Fatalf("round %d left %d window flights, %d table flights, %d walks", round, wf, tf, WalksInFlight())
		}
		for _, name := range names { // nothing a cancellation abandoned blocks a rebuild
			b, _, err := plan.Resource(ctx, name)
			if err != nil || !bytes.Equal(b, want[name]) {
				t.Fatalf("round %d rebuild %s: err %v", round, name, err)
			}
			h, err := plan.Open(ctx, name)
			if err != nil {
				t.Fatalf("round %d reopen %s: %v", round, name, err)
			}
			if _, err := h.WriteTo(ctx, io.Discard); err != nil {
				t.Fatalf("round %d rewrite %s: %v", round, name, err)
			}
		}
	}
	if cancelled != 50 {
		t.Fatalf("%d clients cancelled, want 50", cancelled)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		buf := make([]byte, 1<<20)
		t.Fatalf("%d goroutines before, %d after:\n%s", base, n, buf[:runtime.Stack(buf, true)])
	}
	t.Logf("%d cancelled (%d ended in context.Canceled, the rest were served), %d served exactly, goroutines back to %d", cancelled, cancelledErr, served, base)
}
