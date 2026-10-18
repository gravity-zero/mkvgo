package mp4

import (
	"context"
	"math"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// peekHead reads from the first cued cluster until every track has its first
// sample: the first PTS anchors the DTS timeline, and the sample entry is
// built for codecs that derive it from the first frame.
func (p *HLSPlan) peekHead(ctx context.Context) error {
	// Grid probe for laced audio with no DefaultDuration: the reader collapses a
	// lace onto its block timecode, so the plan recovers the stride the same way
	// the full pass does - from the first two distinct block timecodes and the
	// first block's frame count - and the init's total duration matches.
	type gridProbe struct {
		firstTC, frames, secondTC int64
		haveFirst, haveSecond     bool
		single                    bool // the first block held one frame: nothing to derive
	}
	probes := map[*planTrack]*gridProbe{}
	// Leading PTS in decode order, per track: the composition shift is measured
	// over the same bounded prefix the full pass measures it over, so the plan's
	// init carries the same edit list - byte-identical, without ever seeing more
	// than the head of the file.
	needFirst := len(p.tracks)
	needGrid := 0
	for _, pt := range p.tracks {
		mk := &pt.ft.outTrack.mkv
		if mk.Type == mkv.AudioTrack && mk.DefaultDurationNs <= 0 {
			probes[pt] = &gridProbe{}
			needGrid++
		}
	}
	err := p.walkBlocks(ctx, p.offsets[0], -1, false, func(b mkv.Block, pt *planTrack) (bool, error) {
		if pt.firstPtsMs < 0 {
			data, err := pt.ft.outTrack.mkv.DecodePayload(b.Data)
			if err != nil {
				return false, err
			}
			pt.ft.outTrack.learnFrame(data)
			if pt.ft.outTrack.sampleEntry == nil {
				entry, err := pt.ft.outTrack.spec.sampleEntry(&pt.ft.outTrack.mkv, data)
				if err != nil {
					return false, err
				}
				pt.ft.outTrack.sampleEntry = entry
			}
			pt.firstPtsMs = b.Timecode
			needFirst--
		}
		if pr := probes[pt]; pr != nil && !pr.haveSecond && !pr.single {
			switch {
			case !pr.haveFirst:
				pr.firstTC, pr.frames, pr.haveFirst = b.BlockTimecode, 1, true
			case b.BlockTimecode == pr.firstTC:
				pr.frames++
			default:
				pr.secondTC, pr.haveSecond = b.BlockTimecode, true
				needGrid--
			}
		}
		// A lace hands out all its frames in a row: once another track's
		// block follows a probed track's first block, that block is complete.
		// Holding a single frame, it has no collapsed lace to recover a stride
		// from (deriveGridTS needs two or more frames on one timecode, and
		// returns 0 otherwise, in the full pass too): the probe is settled
		// there, instead of waiting for the track's NEXT block - which a
		// source storing that track far from the video keeps hundreds of
		// megabytes on (480 MB on a real 4 GB file).
		for opt, pr := range probes {
			if opt != pt && pr.haveFirst && !pr.haveSecond && !pr.single && pr.frames == 1 {
				pr.single = true
				needGrid--
			}
		}
		return needFirst > 0 || needGrid > 0, nil
	})
	if err != nil {
		return err
	}
	for pt, pr := range probes {
		if !pr.haveSecond {
			continue
		}
		pt.gridTS = laceGridTS(pt.ft.outTrack, int(pr.frames)+1, func(i int) int64 {
			if int64(i) < pr.frames {
				return pr.firstTC
			}
			return pr.secondTC
		}, pt.ft.timescale)
		pt.laceSecondMs, pt.laceFirstFrames = pr.secondTC, pr.frames
	}
	return nil
}

// peekTail reads from the last cued cluster to EOF and returns each track's
// two largest PTS (the final sample and its predecessor), -1 when unseen, plus
// the frame count of the final Block (frames sharing the largest block
// timecode). lastFrames lets the grid duration correct a collapsed no-
// DefaultDuration lace, whose frames all report the block timecode as their PTS.
//
// A track absent from that tail ended earlier: it is looked for one segment
// back at a time, header-only, so its final pair is its own, not invented.
func (p *HLSPlan) peekTail(ctx context.Context, off int64) (lastPts, prevPts, lastFrames []int64, err error) {
	lastPts = make([]int64, len(p.tracks))
	prevPts = make([]int64, len(p.tracks))
	lastFrames = make([]int64, len(p.tracks))
	lastBlockTC := make([]int64, len(p.tracks))
	missing := make([]bool, len(p.tracks))
	for i := range lastPts {
		lastPts[i], prevPts[i], lastBlockTC[i], missing[i] = -1, -1, -1, true
	}
	idx := map[*planTrack]int{}
	for i, pt := range p.tracks {
		idx[pt] = i
	}
	fold := func(b mkv.Block, pt *planTrack) (bool, error) {
		i := idx[pt]
		if !missing[i] {
			return true, nil
		}
		switch {
		case b.Timecode > lastPts[i]:
			prevPts[i], lastPts[i] = lastPts[i], b.Timecode
		case b.Timecode > prevPts[i]:
			prevPts[i] = b.Timecode
		}
		switch {
		case b.BlockTimecode > lastBlockTC[i]:
			lastBlockTC[i], lastFrames[i] = b.BlockTimecode, 1
		case b.BlockTimecode == lastBlockTC[i]:
			lastFrames[i]++
		}
		return true, nil
	}
	if err = p.walkBlocks(ctx, off, -1, true, fold); err != nil {
		return nil, nil, nil, err
	}
	starts := append([]int64(nil), p.offsets...)
	if n := len(starts); n == 0 || starts[n-1] < off {
		starts = append(starts, off)
	}
	for k := len(starts) - 2; k >= 0; k-- {
		absent := false
		for i := range missing {
			missing[i] = lastPts[i] < 0
			absent = absent || missing[i]
		}
		if !absent {
			break
		}
		if err = p.walkBlocks(ctx, starts[k], starts[k+1], true, fold); err != nil {
			return nil, nil, nil, err
		}
	}
	return lastPts, prevPts, lastFrames, nil
}

// walkBlocks runs fn over the media-track blocks from the cluster at off until fn returns false,
// a block at or past end (when end > 0) or the stream ends; headerOnly hands fn timecodes and sizes, never payloads.
func (p *HLSPlan) walkBlocks(ctx context.Context, off, end int64, headerOnly bool, fn func(mkv.Block, *planTrack) (bool, error)) error {
	return p.walkBlocksPos(ctx, off, end, headerOnly, func(b mkv.Block, pt *planTrack, _ reader.BlockPos) (bool, error) { return fn(b, pt) })
}

// walkBlocksPos is walkBlocks handing fn each block's position as well.
func (p *HLSPlan) walkBlocksPos(ctx context.Context, off, end int64, headerOnly bool, fn func(mkv.Block, *planTrack, reader.BlockPos) (bool, error)) error {
	src, err := p.openSource(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	if h, ok := src.(mkv.RangeHinter); ok {
		// Unbounded, the walk runs until fn has seen enough: up to the next window's cluster, or the end past the last.
		to := end
		if to < 0 {
			to = math.MaxInt64
			if i := sort.Search(len(p.offsets), func(k int) bool { return p.offsets[k] > off }); i < len(p.offsets) {
				to = p.offsets[i] + windowPeekBytes
			}
		}
		h.HintRange(off, to)
	}
	br, err := reader.NewBlockReaderAt(src, p.tcScale, off)
	if err != nil {
		return err
	}
	defer p.countWalk(&br)()
	br.SetTrackDefaultDurations(p.trackDurs)
	br.SetHeaderOnly(headerOnly)
	routing := make(map[uint64]*planTrack, len(p.tracks))
	for _, pt := range p.tracks {
		routing[pt.ft.outTrack.mkv.ID] = pt
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if p.walkEnd(err) { // clean end, incl. a truncated/over-declared tail
			return nil
		}
		if err != nil {
			return errf("read block: %w", err)
		}
		if end > 0 && br.Pos().Off >= end {
			return nil
		}
		pt, ok := routing[b.TrackNumber]
		if !ok {
			continue
		}
		more, err := fn(b, pt, br.Pos())
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// walkVideoHeadersOnly walks the video track's blocks from the presentation
// start to EOF, structure only: SetHeaderOnly skips the payload of every
// (real-world universal) unlaced block instead of reading it, and KeepTracks
// seek-skips every other track's blocks entirely - so the cost is bounded by
// the video track's block-header count, never by any payload bytes.
func (p *HLSPlan) walkVideoHeadersOnly(ctx context.Context, fn func(mkv.Block) (bool, error)) error {
	vid := p.tracks[p.videoIndex()].ft.outTrack.mkv.ID
	src, err := p.openSource(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	br, err := reader.NewBlockReaderAt(src, p.tcScale, p.offsets[0])
	if err != nil {
		return err
	}
	defer p.countWalk(&br)()
	br.SetTrackDefaultDurations(p.trackDurs)
	br.KeepTracks(vid)
	br.SetHeaderOnly(true)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.Next()
		if p.walkEnd(err) { // clean end, incl. a truncated/over-declared tail
			return nil
		}
		if err != nil {
			return errf("read block: %w", err)
		}
		more, err := fn(b)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// synthesizeVideoCues is Options.SynthesizeIndex's one-time walk: the cue
// points a healthy index would carry - one per cluster holding a video
// keyframe, at the keyframe's stored timecode - built in memory from a
// structure-only pass (KeepTracks + SetHeaderOnly: block headers, never
// payload bytes). The cost is the walk a persistent repair would pay anyway,
// but nothing is written: the only road to seekable on-demand playback for a
// source on a read-only mount. A structurally broken cluster stream still
// refuses (repair or salvage first).
func synthesizeVideoCues(ctx context.Context, srcPath string, fs *mkv.FS, c *mkv.Container, vidID uint64, trackDurs map[uint64]int64) ([]mkv.CuePoint, error) {
	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
	if err != nil {
		return nil, errf("synthesize index: %w", err)
	}
	br.SetTrackDefaultDurations(trackDurs)
	br.KeepTracks(vidID)
	br.SetHeaderOnly(true)
	var cues []mkv.CuePoint
	lastCluster := int64(-1)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := br.Next()
		if isBlockWalkEnd(err) {
			return cues, nil
		}
		if err != nil {
			return nil, errf("synthesize index: %w (repair the source first with a resyncing reindex)", err)
		}
		if !b.Keyframe {
			continue
		}
		off := br.ClusterOffset()
		if off == lastCluster {
			continue // one cue per cluster, like real muxers write
		}
		lastCluster = off
		cues = append(cues, mkv.CuePoint{
			TimeMs:     b.BlockTimecode,
			Track:      vidID,
			ClusterPos: off - c.SegmentStart,
		})
	}
}

// probeCompositionShift measures the video track's composition shift - what the
// trun's offsets are pushed up by, and the init's edit list takes back out - from
// the opening frames alone, HEADER-ONLY: block headers carry the timecodes and the
// keyframe flag, and no sample byte is read. The full pass measures the same shift
// over the same prefix of the same track (compositionShiftTS), so the two inits
// stay byte-identical while this one costs a handful of headers, not a GOP of
// video.
func (p *HLSPlan) probeCompositionShift(ctx context.Context) error {
	pt := p.tracks[p.videoIndex()]
	var (
		pts  []int64
		sync []bool
	)
	err := p.walkVideoHeadersOnly(ctx, func(b mkv.Block) (bool, error) {
		if len(pts) > 0 && b.Keyframe {
			return false, nil // the GOP closed: the reorder pattern is fully in hand
		}
		pts = append(pts, b.Timecode)
		sync = append(sync, b.Keyframe)
		return len(pts) < compositionPrefix, nil
	})
	if err != nil {
		return err
	}
	scale := tsScale(pt.ft.timescale)
	ts := make([]int64, len(pts))
	for i, ms := range pts {
		ts[i] = scale(ms)
	}
	pt.ft.ctsShiftTS = compositionShiftTS(ts, sync)
	return nil
}
