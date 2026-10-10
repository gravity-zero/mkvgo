package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
