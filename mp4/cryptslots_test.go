package mp4

import (
	"context"
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
