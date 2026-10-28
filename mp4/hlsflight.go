package mp4

import (
	"context"
	"sync"
)

// buildPanicked, deferred in a flight's leader around its build, turns a panic of the build into the build's error:
// the leader then finishes the flight, and its waiters get the error instead of waiting on a flight never finished.
func buildPanicked(err *error, what string) {
	if r := recover(); r != nil {
		*err = errf("%s: the build panicked: %v", what, r)
	}
}

// flight is one window build others wait on: it runs under the leader's
// context kept alive for the waiters, cancelled once the leader has gone and
// no waiter remains, when it also leaves its map so a newcomer starts afresh.
type flight struct {
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	gone    bool
	evict   func()
}

// newFlight watches the leader's context; evict drops the flight from its map and runs under mu.
func newFlight(mu *sync.Mutex, leader context.Context, evict func()) *flight {
	f := &flight{done: make(chan struct{}), evict: evict}
	f.ctx, f.cancel = context.WithCancel(context.WithoutCancel(leader))
	go func() {
		select {
		case <-leader.Done():
			mu.Lock()
			f.gone = true
			f.settle()
			mu.Unlock()
		case <-f.done:
		}
	}()
	return f
}

// join counts a waiter. Caller holds mu.
func (f *flight) join() { f.waiters++ }

// leave drops a waiter that gave up. Caller holds mu.
func (f *flight) leave() {
	f.waiters--
	f.settle()
}

// settle cancels the build once nobody wants it. Caller holds mu.
func (f *flight) settle() {
	if f.gone && f.waiters == 0 {
		f.cancel()
		f.evict()
	}
}

// finish releases the waiters once the result is published.
func (f *flight) finish() {
	close(f.done)
	f.cancel()
}
