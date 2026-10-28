package mp4

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
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

// stallFS reads from base; armed, the first read after it blocks until release closes.
type stallFS struct {
	base    *mkv.FS
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

type stallFile struct {
	mkv.ReadSeekCloser
	fs *stallFS
}

func (f *stallFile) Read(p []byte) (int, error) {
	if f.fs.armed.CompareAndSwap(true, false) { // the first read only: a sync.Once would hold the others behind it
		close(f.fs.entered)
		<-f.fs.release
	}
	return f.ReadSeekCloser.Read(p)
}

func (s *stallFS) fs() *mkv.FS {
	fs := *s.base
	fs.Open = func(p string) (mkv.ReadSeekCloser, error) {
		f, err := s.base.Open(p)
		if err != nil {
			return nil, err
		}
		return &stallFile{ReadSeekCloser: f, fs: s}, nil
	}
	return &fs
}

// TestCryptSlotFreeWhileReading: an encrypted segment held its slot while it read the source; with one slot, a
// read that stalled (a disk asleep, a network share) held every other encrypted segment behind it. Another request is
// served while the first waits on its read.
func TestCryptSlotFreeWhileReading(t *testing.T) {
	defer func(g *cryptOrder) { cryptGate = g }(cryptGate)
	cryptGate = newCryptOrder(1)
	ctx := context.Background()
	data, err := os.ReadFile(interleavedMP4)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []Options{
		{SegmentMs: 2000, Encrypt: &HLSEncryption{Key: []byte("0123456789abcdef"), KeyURI: "k"}},
		{SegmentMs: 2000, CENC: &CENCOptions{Scheme: "cenc", Key: []byte("0123456789abcdef"), KeyID: []byte("0123456789abcdef"), IV: []byte("01234567"), KeyURI: "k"}},
	} {
		m := mkv.NewMemFS()
		m.Put("in.mp4", data)
		sf := &stallFS{base: m.FS(), entered: make(chan struct{}), release: make(chan struct{})}
		o.FS, o.StreamFromFS = sf.fs(), true
		plan, err := PlanHLS(ctx, "in.mp4", o)
		if err != nil {
			t.Fatal(err)
		}
		first, err := plan.Open(ctx, plan.SegmentName(0))
		if err != nil {
			t.Fatal(err)
		}
		second, err := plan.Open(ctx, plan.SegmentName(1))
		if err != nil {
			t.Fatal(err)
		}
		sf.armed.Store(true)
		done := make(chan error, 1)
		go func() { _, err := first.WriteTo(ctx, io.Discard); done <- err }()
		select {
		case <-sf.entered:
		case err := <-done:
			t.Fatalf("the segment was written without a source read after Open (%v)", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the segment never read its source")
		}
		served := make(chan error, 1)
		go func() { _, err := second.WriteTo(ctx, io.Discard); served <- err }()
		select {
		case err := <-served:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a request waiting on its source read held the only slot")
		}
		close(sf.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		first.Close()
		second.Close()
	}
}
