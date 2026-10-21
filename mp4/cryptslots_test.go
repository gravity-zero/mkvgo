package mp4

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The slot goes to the oldest request waiting, not the first to ask; a waiter
// whose request ends gives its place up, and the pool counts every slot back.
func TestCryptOrderOldestFirst(t *testing.T) {
	g := newCryptOrder(1)
	ctx := context.Background()
	if err := g.acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	got := make(chan int64, 3)
	wait := func(seq int64) {
		if err := g.acquire(ctx, seq); err != nil {
			t.Error(err)
			return
		}
		got <- seq
		g.release()
	}
	gone, cancel := context.WithCancel(ctx)
	goneErr := make(chan error)
	waiting := func(n int) func() bool {
		return func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.waiting.Len() == n }
	}
	for i, seq := range []int64{9, 5} { // the younger asks first
		go wait(seq)
		waitFor(t, waiting(i+1))
	}
	go func() { goneErr <- g.acquire(gone, 2) }() // older than both, but leaves
	waitFor(t, waiting(3))
	cancel()
	if err := <-goneErr; err == nil {
		t.Fatal("a waiter whose request ended took a slot")
	}
	g.release()
	if a, b := <-got, <-got; a != 5 || b != 9 {
		t.Fatalf("served %d then %d, want 5 then 9", a, b)
	}
	waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.free == 1 && g.waiting.Len() == 0 })
}

// No more steps run at once than the gate has slots.
func TestCryptOrderCap(t *testing.T) {
	g := newCryptOrder(2)
	var live, peak int
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(seq int64) {
			defer func() { done <- struct{}{} }()
			if err := g.acquire(context.Background(), seq); err != nil {
				t.Error(err)
				return
			}
			g.mu.Lock()
			live++
			peak = max(peak, live)
			g.mu.Unlock()
			time.Sleep(time.Millisecond)
			g.mu.Lock()
			live--
			g.mu.Unlock()
			g.release()
		}(int64(i))
	}
	for i := 0; i < 16; i++ {
		<-done
	}
	if peak > 2 {
		t.Fatalf("%d steps at once under 2 slots", peak)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

// blockingWriter blocks its first write until released.
type blockingWriter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return len(p), nil
}

// An encrypted segment whose client stops reading holds no slot: with one
// slot, another request is served while the first is blocked on its write.
func TestCryptSlotFreeWhileWriting(t *testing.T) {
	defer func(g *cryptOrder) { cryptGate = g }(cryptGate)
	cryptGate = newCryptOrder(1)
	ctx := context.Background()
	for _, o := range []Options{
		{SegmentMs: 2000, Encrypt: &HLSEncryption{Key: []byte("0123456789abcdef"), KeyURI: "k"}},
		{SegmentMs: 2000, CENC: &CENCOptions{Scheme: "cenc", Key: []byte("0123456789abcdef"), KeyID: []byte("0123456789abcdef"), IV: []byte("01234567"), KeyURI: "k"}},
	} {
		plan, err := PlanHLS(ctx, interleavedMP4, o)
		if err != nil {
			t.Fatal(err)
		}
		slow := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			h, err := plan.Open(ctx, plan.SegmentName(0))
			if err == nil {
				_, err = h.WriteTo(ctx, slow)
				h.Close()
			}
			done <- err
		}()
		<-slow.entered
		h, err := plan.Open(ctx, plan.SegmentName(1))
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { _, err := h.WriteTo(ctx, io.Discard); served <- err }()
		select {
		case err := <-served:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a request blocked on its client held the only slot")
		}
		h.Close()
		close(slow.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// The encryption slots are the processors the process may use (GOMAXPROCS,
// which follows a container's CPU limit), read in a process started with 2.
func TestCryptGateFollowsGOMAXPROCS(t *testing.T) {
	if os.Getenv("MKVGO_CRYPTGATE_CHILD") == "1" {
		fmt.Printf("slots=%d\n", cryptGate.free)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCryptGateFollowsGOMAXPROCS$")
	cmd.Env = append(os.Environ(), "MKVGO_CRYPTGATE_CHILD=1", "GOMAXPROCS=2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "slots=2\n") {
		t.Fatalf("encryption slots under GOMAXPROCS 2: %s", out)
	}
}
