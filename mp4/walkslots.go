package mp4

import (
	"context"
	"sync"
	"sync/atomic"
)

// walkSlots caps the windows the process builds at once (SetMaxConcurrentWalks); nil lifts the cap.
var (
	walkMu    sync.Mutex
	walkSlots chan struct{}
	walksLive atomic.Int64
)

// SetMaxConcurrentWalks caps, process-wide, how many windows the on-demand plans build at once; a window in flight holds its media, so this bounds that memory to n windows and queues the rest. 0 lifts the cap.
func SetMaxConcurrentWalks(n int) {
	walkMu.Lock()
	defer walkMu.Unlock()
	if n <= 0 {
		walkSlots = nil
		return
	}
	walkSlots = make(chan struct{}, n)
}

// WalksInFlight reports how many windows the process is building right now.
func WalksInFlight() int64 { return walksLive.Load() }

// acquireWalk takes a slot, waiting while the cap is reached, and returns its release; waited says the cap made it queue.
func acquireWalk(ctx context.Context) (release func(), waited bool, err error) {
	walkMu.Lock()
	slots := walkSlots
	walkMu.Unlock()
	if slots != nil {
		select {
		case slots <- struct{}{}:
		default:
			waited = true
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}
	}
	walksLive.Add(1)
	return func() {
		walksLive.Add(-1)
		if slots != nil {
			<-slots
		}
	}, waited, nil
}
