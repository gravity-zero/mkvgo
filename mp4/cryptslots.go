package mp4

import (
	"container/heap"
	"context"
	"runtime"
	"sync"
	"sync/atomic"
)

// cryptGate lets as many encrypted segments work at once as there are processors, the oldest request first: each
// holds its slot but while writing to its client, so they finish in turn and a slow client holds none.
var cryptGate = newCryptOrder(runtime.GOMAXPROCS(0))

// cryptSeq orders the encrypted writes by when they started.
var cryptSeq atomic.Int64

type cryptOrder struct {
	mu      sync.Mutex
	free    int
	waiting cryptWaiters
}

type cryptWaiter struct {
	seq   int64
	ready chan struct{}
	gone  bool // its request ended while it waited
}

type cryptWaiters []*cryptWaiter

func (w cryptWaiters) Len() int           { return len(w) }
func (w cryptWaiters) Less(i, j int) bool { return w[i].seq < w[j].seq }
func (w cryptWaiters) Swap(i, j int)      { w[i], w[j] = w[j], w[i] }
func (w *cryptWaiters) Push(x any)        { *w = append(*w, x.(*cryptWaiter)) }
func (w *cryptWaiters) Pop() any {
	old := *w
	x := old[len(old)-1]
	*w = old[:len(old)-1]
	return x
}

func newCryptOrder(n int) *cryptOrder { return &cryptOrder{free: max(n, 1)} }

// acquire takes a slot for the request started seq-th, waiting behind older requests; it fails when ctx ends first.
func (g *cryptOrder) acquire(ctx context.Context, seq int64) error {
	g.mu.Lock()
	if g.free > 0 {
		g.free--
		g.mu.Unlock()
		return nil
	}
	w := &cryptWaiter{seq: seq, ready: make(chan struct{})}
	heap.Push(&g.waiting, w)
	g.mu.Unlock()
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()
		select {
		case <-w.ready: // handed a slot as it gave up: pass it on
			g.handOff()
		default:
			w.gone = true
		}
		return ctx.Err()
	}
}

// release gives the slot to the oldest request waiting, else back to the pool.
func (g *cryptOrder) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.handOff()
}

// handOff gives one slot away. Caller holds mu.
func (g *cryptOrder) handOff() {
	for g.waiting.Len() > 0 {
		w := heap.Pop(&g.waiting).(*cryptWaiter)
		if !w.gone {
			close(w.ready)
			return
		}
	}
	g.free++
}
