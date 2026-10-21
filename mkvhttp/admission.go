package mkvhttp

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
)

// gate admits Options.MaxInFlight requests at once and queues Options.MaxWaiting more.
type gate struct {
	slots      chan struct{}
	waiting    atomic.Int64
	maxWaiting int64
}

func newGate(o Options) *gate {
	if o.MaxInFlight <= 0 {
		return nil
	}
	return &gate{slots: make(chan struct{}, o.MaxInFlight), maxWaiting: int64(max(o.MaxWaiting, 0))}
}

// enter takes a slot, waiting in the queue while it has room; false when the queue is full or ctx ends first.
func (g *gate) enter(ctx context.Context) (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
		return g.leave, true
	default:
	}
	if g.waiting.Add(1) > g.maxWaiting {
		g.waiting.Add(-1)
		return nil, false
	}
	defer g.waiting.Add(-1)
	select {
	case g.slots <- struct{}{}:
		return g.leave, true
	case <-ctx.Done():
		return nil, false
	}
}

func (g *gate) leave() { <-g.slots }

// gated reports whether serving name takes a slot: playlists and manifests are in memory and never wait.
func gated(name string) bool {
	return !strings.HasSuffix(name, ".m3u8") && !strings.HasSuffix(name, ".mpd")
}

// busy answers a request the gate turned away; no cache may keep the refusal.
func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "mkvhttp: server busy, retry", http.StatusServiceUnavailable)
}
