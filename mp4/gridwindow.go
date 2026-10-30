package mp4

// Grid-timed audio, one segment window at a time. A window's frames are numbered on the grid from where it opens, one
// slot per lace frame, a block's own slot when it lies ahead, never stepping back. Where a window opens and where the
// next one does are functions of the window and its two neighbours alone, so the full pass and an on-demand plan,
// in any request order, time every window alike; a delay the numbering builds up (timecodes running ahead of the grid
// for a while) is given back inside the window, which ends where the next one opens.

// gridSpan is where a grid-timed window opens and where the window after it opens, as frame indexes; end is -1 for
// the track's last window, which ends on its last block's own slots.
type gridSpan struct{ open, end int64 }

var noGridSpan = gridSpan{-1, -1}

// gridFrames numbers n frames from open (the first frame's own slot when negative) into ks: it returns the index past
// the last frame and the first frame held later than its own slot (n when none).
func gridFrames(n int, blockPts func(int) int64, slot func(int64) int64, open int64, ks []int64) (end int64, late int) {
	late = n
	var k, raw int64
	for i := 0; i < n; i++ {
		switch {
		case i == 0:
			raw = slot(blockPts(0))
			k = raw
			if open >= 0 {
				k = open
			}
		case blockPts(i) != blockPts(i-1):
			raw = slot(blockPts(i))
			k = max(raw, k+1)
		default:
			raw++
			k++
		}
		ks[i] = k
		if k > raw && late == n {
			late = i
		}
	}
	return k + 1, late
}

// gridColdEnd is the index past a window's last frame when it opens on its first block's own slot.
func gridColdEnd(n int, blockPts func(int) int64, slot func(int64) int64) int64 {
	end, _ := gridFrames(n, blockPts, slot, -1, make([]int64, n))
	return end
}

// gridJitter reports whether a window's blocks leave the grid, numbered from its first block's own slot: a timecode
// stepping back or jumping ahead of the next slot.
func gridJitter(n int, blockPts func(int) int64, slot func(int64) int64) bool {
	var k int64
	for i := 0; i < n; i++ {
		switch {
		case i == 0:
			k = slot(blockPts(0))
		case blockPts(i) != blockPts(i-1):
			raw := slot(blockPts(i))
			if raw != k+1 {
				return true
			}
			k = raw
		default:
			k++
		}
	}
	return false
}

// gridOpen is where a window opens: its first block's own slot, or, when its blocks leave the grid (a run of
// timecodes behind it may straddle the boundary), no earlier than the window before ends opened on its own slot
// (prevEnd, -1 when that window holds no frame of the track).
func gridOpen(n int, blockPts func(int) int64, slot func(int64) int64, prevEnd int64) int64 {
	open := slot(blockPts(0))
	if prevEnd > open && gridJitter(n, blockPts, slot) {
		return prevEnd
	}
	return open
}

// gridTrackEnd is the index past the track's last frame: its last block's own slot, one per frame it holds.
func gridTrackEnd(n int, blockPts func(int) int64, slot func(int64) int64) int64 {
	last := blockPts(n - 1)
	frames := int64(0)
	for i := n - 1; i >= 0 && blockPts(i) == last; i-- {
		frames++
	}
	return slot(last) + frames
}

// gridTime times n frames opening on span.open and lasting up to span.end (the track's end when -1): each frame's
// decode time and duration go to set. Frames numbered past the end give the excess back from the first late frame
// on (the window's last when none is), or from the last frame before it with room enough, every later decode time
// scaled into what is left; a window holding more frames than ticks keeps its numbering, its last frame one tick.
func gridTime(n int, blockPts func(int) int64, slot func(int64) int64, gridTS int64, span gridSpan, set func(i int, dts, dur int64)) {
	ks := make([]int64, n)
	natural, late := gridFrames(n, blockPts, slot, span.open, ks)
	end := span.end
	if end < 0 {
		end = gridTrackEnd(n, blockPts, slot)
	}
	dts := make([]int64, n)
	for i := range ks {
		dts[i] = ks[i] * gridTS
	}
	if natural > end {
		j := min(late, n-1)
		fits := func(j int) bool { return end > ks[j] && (end-ks[j])*gridTS >= natural-ks[j] }
		for j > 0 && !fits(j) {
			j--
		}
		if k0 := ks[j]; fits(j) {
			for i := j; i < n; i++ {
				dts[i] = k0*gridTS + (ks[i]-k0)*gridTS*(end-k0)/(natural-k0)
			}
		}
	}
	for i := 0; i < n-1; i++ {
		set(i, dts[i], dts[i+1]-dts[i])
	}
	set(n-1, dts[n-1], max(end*gridTS-dts[n-1], 1))
}

// gridTimeWindows times a whole grid-timed track window by window, the windows cut at bounds (ms) as segmentWindow
// cuts them: what the on-demand plan derives for each window alone.
func gridTimeWindows(samples []fragSample, bounds []int64, scale func(int64) int64, gridTS int64) {
	anchor := scale(samples[0].blockPtsMs)
	slot := func(ms int64) int64 { return gridIndex(scale(ms)-anchor, gridTS) }
	type window struct{ from, to int }
	wins := make([]window, len(bounds))
	j := 0
	for k := range bounds {
		segEnd := int64(1<<63 - 1)
		if k+1 < len(bounds) {
			segEnd = bounds[k+1]
		}
		from := j
		for j < len(samples) && samples[j].blockPtsMs < segEnd {
			j++
		}
		wins[k] = window{from, j}
	}
	pts := func(w window) func(int) int64 { return func(i int) int64 { return samples[w.from+i].blockPtsMs } }
	open := make([]int64, len(wins))
	for k, w := range wins {
		if w.to == w.from {
			continue
		}
		prevEnd := int64(-1)
		if k > 0 && wins[k-1].to > wins[k-1].from {
			pw := wins[k-1]
			prevEnd = gridColdEnd(pw.to-pw.from, pts(pw), slot)
		}
		open[k] = gridOpen(w.to-w.from, pts(w), slot, prevEnd)
	}
	end := int64(-1)
	for k := len(wins) - 1; k >= 0; k-- {
		w := wins[k]
		if w.to == w.from {
			continue
		}
		gridTime(w.to-w.from, pts(w), slot, gridTS, gridSpan{open[k], end}, func(i int, dts, dur int64) {
			s := &samples[w.from+i]
			s.dtsTS, s.durTS, s.ctsTS = dts, dur, 0
		})
		end = open[k]
	}
}
