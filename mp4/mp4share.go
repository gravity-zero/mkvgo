package mp4

import (
	"context"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A remote MP4 window's video read takes the span that holds its audio too, in one request, and keeps the audio it
// passed (every rendition) for the window's audio segments, within the window budget and for shareTTL.
var (
	shareTTL   = 30 * time.Second
	shareGrace = 10 * time.Millisecond // how long an audio segment waits for its window's video read to begin: 30 ms shared no more and cost a lone audio segment 20 ms
	shareReach = 1.5                   // the most the span with the audio may be of the video's: past it the audio lies elsewhere
)

// mp4Share holds, per window, the audio samples a video read kept, or the read in flight.
type mp4Share struct {
	mu      sync.Mutex
	wins    map[int]*sharedWin
	order   []int         // windows in the order they were read, for expiry and reclaim
	started chan struct{} // closed, and replaced, each time a read begins
}

// sharedWin is one window's read: done is closed when it ended; runs are the audio bytes it kept, nil when none.
type sharedWin struct {
	done  chan struct{}
	runs  []sharedRun
	held  int64
	at    time.Time
	timer *time.Timer  // gives the window back at shareTTL
	owed  map[int]bool // the audio renditions not served from it yet: the window goes when none is left
}

// sharedRun is the source's bytes [off, off+len(data)).
type sharedRun struct {
	off  int64
	data []byte
}

// shareable reports whether the plan's windows are read for their audio: a remote MP4 (a CENC segment reads its
// samples its own way).
func (p *HLSPlan) shareable(src io.Reader) bool {
	_, local := src.(*os.File)
	return p.mp4src && p.opts.CENC == nil && !local && p.onNetwork(src)
}

// audioRuns is where window win's audio samples lie, every audio rendition, back-to-back samples merged.
func (p *HLSPlan) audioRuns(win int) []sharedRun {
	type piece struct{ off, n int64 }
	var all []piece
	for ti := range p.tracks {
		if p.tracks[ti].ft.outTrack.spec.video {
			continue
		}
		seg, offs := p.mp4Segment(ti, win)
		for x, s := range seg.samples {
			all = append(all, piece{offs[x], int64(s.size)})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].off < all[j].off })
	var runs []sharedRun
	var ends []int64
	for _, pc := range all {
		if k := len(runs) - 1; k >= 0 && pc.off <= ends[k] {
			ends[k] = max(ends[k], pc.off+pc.n)
			continue
		}
		runs = append(runs, sharedRun{off: pc.off})
		ends = append(ends, pc.off+pc.n)
	}
	for k := range runs {
		runs[k].data = make([]byte, 0, ends[k]-runs[k].off)
	}
	return runs
}

// expire drops the kept windows past shareTTL. Caller holds mu.
func (s *mp4Share) expire(now time.Time) {
	live := s.order[:0]
	for _, win := range s.order {
		w := s.wins[win]
		if w == nil {
			continue
		}
		if w.runs != nil && now.Sub(w.at) > shareTTL {
			s.drop(win, w)
			continue
		}
		live = append(live, win)
	}
	s.order = live
}

// drop forgets a window and gives its bytes back, to the budget and to the collector. Caller holds mu.
func (s *mp4Share) drop(win int, w *sharedWin) {
	delete(s.wins, win)
	windowBytesTotal.Add(-w.held)
	w.held, w.runs = 0, nil
	if w.timer != nil {
		w.timer.Stop()
	}
}

// begin opens window win's read; false when one is in flight or its audio is kept.
func (s *mp4Share) begin(win int) (*sharedWin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(time.Now())
	if s.wins == nil {
		s.wins = map[int]*sharedWin{}
	}
	if _, busy := s.wins[win]; busy {
		return nil, false
	}
	w := &sharedWin{done: make(chan struct{}), owed: map[int]bool{}}
	s.wins[win] = w
	s.order = append(s.order, win)
	if s.started != nil {
		close(s.started)
	}
	s.started = make(chan struct{})
	return w, true
}

// end closes window win's read: its audio kept when runs is not nil, else forgotten.
func (s *mp4Share) end(win int, w *sharedWin, runs []sharedRun, held int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runs != nil {
		w.runs, w.held, w.at = runs, held, time.Now()
		w.timer = time.AfterFunc(shareTTL, func() { // an idle plan gives its window back too
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.wins[win] == w {
				s.drop(win, w)
			}
		})
	} else {
		windowBytesTotal.Add(-held)
		if s.wins[win] == w {
			delete(s.wins, win)
		}
	}
	close(w.done)
}

// reclaim gives back the oldest kept windows until need fits the process budget; it reports whether it gave any.
func (s *mp4Share) reclaim(need int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	freed := false
	for _, win := range s.order {
		if windowBytesTotal.Load()+need <= windowBytesBudget.Load() {
			break
		}
		if w := s.wins[win]; w != nil && w.runs != nil {
			s.drop(win, w)
			freed = true
		}
	}
	return freed
}

// lookup returns window win's kept audio, waiting for its read to end, or up to grace for one to begin; nil when
// there is none.
func (s *mp4Share) lookup(ctx context.Context, win int, grace time.Duration) []sharedRun {
	deadline := time.Now().Add(grace)
	for {
		s.mu.Lock()
		s.expire(time.Now())
		w := s.wins[win]
		if s.started == nil {
			s.started = make(chan struct{})
		}
		started := s.started
		s.mu.Unlock()
		if w != nil {
			select {
			case <-w.done:
			case <-ctx.Done():
				return nil
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return w.runs // nil when the read kept nothing, or the window was dropped since
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		timer := time.NewTimer(left)
		select {
		case <-started:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if ctx.Err() != nil {
			return nil
		}
	}
}

// copyShared writes a whole video segment of window win reading the window's span once, its audio samples kept
// for the window's audio segments; false (nothing written) when the window cannot be shared.
func (p *HLSPlan) copyShared(ctx context.Context, w io.Writer, c *spanCopier, src io.ReadSeeker, pieces []span, win int) (bool, error) {
	if len(pieces) == 0 {
		return false, nil
	}
	for i, pc := range pieces {
		if pc.emit != nil || (i > 0 && pc.off < pieces[i-1].off+pieces[i-1].n) {
			return false, nil // the frames are not in file order: read as before
		}
	}
	runs := p.audioRuns(win)
	if len(runs) == 0 {
		return false, nil
	}
	lo, hi, near := withinReach(pieces, runs)
	if !near {
		return false, nil // the audio lies away from the video: reading it along would read more than it saves
	}
	var held int64
	for _, r := range runs {
		held += int64(cap(r.data))
	}
	sw, ok := p.share.begin(win)
	if !ok {
		return false, nil
	}
	for ti := range p.tracks {
		if !p.tracks[ti].ft.outTrack.spec.video {
			sw.owed[ti] = true // set before the read ends: no reader looks at it until done is closed
		}
	}
	registerKeeper(p)
	if kept := reserveWindowBytes(held) || p.reclaimKept(held) && reserveWindowBytes(held); !kept {
		p.share.end(win, sw, nil, 0)
		p.winMu.Lock()
		p.stats.KeptFallbacks++
		p.winMu.Unlock()
		return false, nil
	}
	err := p.readThrough(ctx, w, c, src, pieces, runs, lo, hi)
	for _, r := range runs {
		if err == nil && len(r.data) < cap(r.data) {
			err = errf("the shared read of window %d left an audio run short", win) // the source ended early: nothing kept
		}
	}
	if err != nil {
		p.share.end(win, sw, nil, held)
		return true, err
	}
	p.share.end(win, sw, runs, held)
	return true, nil
}

// readThrough reads the source's [lo, hi) once, writing the pieces' bytes to w and filling runs as it passes them.
func (p *HLSPlan) readThrough(ctx context.Context, w io.Writer, c *spanCopier, src io.ReadSeeker, pieces []span, runs []sharedRun, lo, hi int64) error {
	if h, ok := src.(mkv.RangeHinter); ok {
		h.HintRange(lo, hi)
	}
	if _, err := src.Seek(lo, io.SeekStart); err != nil {
		return err
	}
	pos, k, r := lo, 0, 0
	for pos < hi {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := io.ReadFull(src, c.buf[:min(int64(len(c.buf)), hi-pos)])
		c.count(int64(m))
		chunk := c.buf[:m]
		end := pos + int64(m)
		for ; r < len(runs); r++ {
			run := &runs[r]
			from, to := max(pos, run.off+int64(len(run.data))), min(end, run.off+int64(cap(run.data)))
			if from < to {
				run.data = append(run.data, chunk[from-pos:to-pos]...)
			}
			if run.off+int64(cap(run.data)) > end {
				break
			}
		}
		for ; k < len(pieces); k++ {
			pc := pieces[k]
			from, to := max(pos, pc.off), min(end, pc.off+pc.n)
			if from < to {
				if err := c.queue(w, chunk[from-pos:to-pos]); err != nil {
					return err
				}
			}
			if pc.off+pc.n > end {
				break
			}
		}
		if err := c.flush(w); err != nil { // the queued writes are slices of the buffer read next
			return err
		}
		pos = end
		if err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// served marks rendition ti of window win served from the window, which goes once every rendition was.
func (s *mp4Share) served(win, ti int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.wins[win]; w != nil && w.runs != nil {
		delete(w.owed, ti)
		if len(w.owed) == 0 {
			s.drop(win, w)
		}
	}
}

// copyFromShare writes audio rendition ti's segment of window win from the audio its video read kept; false
// (nothing written) when the window kept none, or not every sample.
func (p *HLSPlan) copyFromShare(ctx context.Context, w io.Writer, c *spanCopier, pieces []span, win, ti int) (bool, error) {
	runs := p.share.lookup(ctx, win, shareGrace)
	if runs == nil {
		return false, nil
	}
	find := func(pc span) []byte {
		k := sort.Search(len(runs), func(i int) bool { return runs[i].off+int64(len(runs[i].data)) > pc.off })
		if k == len(runs) || runs[k].off > pc.off || pc.off+pc.n > runs[k].off+int64(len(runs[k].data)) {
			return nil
		}
		return runs[k].data[pc.off-runs[k].off : pc.off-runs[k].off+pc.n]
	}
	for _, pc := range pieces {
		if pc.emit == nil && find(pc) == nil {
			return false, nil
		}
	}
	for _, pc := range pieces {
		b := pc.emit
		if b == nil {
			b = find(pc)
		}
		if err := c.queue(w, b); err != nil {
			return true, err
		}
	}
	p.winMu.Lock()
	p.stats.KeptSegments++
	p.winMu.Unlock()
	err := c.flush(w)
	p.share.served(win, ti) // after the write: its slices of the window are written out
	return true, err
}

// shareWindow serves a remote MP4 segment through its window's shared read: a whole video segment reads it, an audio
// segment takes its kept audio; false (nothing written) when it is served the usual way.
func (p *HLSPlan) shareWindow(ctx context.Context, w io.Writer, c *spanCopier, src io.ReadSeeker, pieces []span, win, ti int, whole bool) (bool, error) {
	if !c.exact {
		if !whole {
			return false, nil
		}
		return p.copyShared(ctx, w, c, src, pieces, win)
	}
	return p.copyFromShare(ctx, w, c, pieces, win, ti)
}

// dropAll gives every kept window back to the budget.
func (s *mp4Share) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for win, w := range s.wins {
		if w.runs != nil {
			s.drop(win, w)
		}
	}
}

// withinReach is the span [lo, hi) holding a window's video pieces and its audio runs; near when it is at most
// shareReach times the video's own.
func withinReach(pieces []span, runs []sharedRun) (lo, hi int64, near bool) {
	last := pieces[len(pieces)-1]
	lo = min(pieces[0].off, runs[0].off)
	hi = max(last.off+last.n, runs[len(runs)-1].off+int64(cap(runs[len(runs)-1].data)))
	return lo, hi, float64(hi-lo) <= shareReach*float64(last.off+last.n-pieces[0].off)
}
