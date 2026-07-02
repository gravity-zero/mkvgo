package ops

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const defaultClusterDurationMs = 1000

type streamOpts struct {
	remap      map[uint64]uint64
	timeOffset int64
	// trackOffsets, when non-nil, shifts each block by a PER-OUTPUT-TRACK offset
	// (keyed by the remapped track id) instead of the single timeOffset. Join uses
	// it so each track is concatenated against its own end, avoiding the A/V drift
	// a single per-file offset causes when tracks end at slightly different times.
	trackOffsets map[uint64]int64
	// trackEnds, when non-nil, is filled with each output track's next free start
	// timecode (its last frame's end), so the caller can use it as the next file's
	// trackOffsets. Independent of trackOffsets.
	trackEnds     map[uint64]int64
	timeStart     int64
	timeEnd       int64
	keyframeAlign bool // split on keyframe boundaries
	// videoTracks holds the SOURCE track numbers of video tracks. keyframeAlign
	// aligns on these: every audio block is flagged keyframe, so aligning on
	// "any keyframe" would start a segment mid-GOP (corrupt video until the
	// next real video keyframe). Empty means no video track: any keyframe cuts.
	videoTracks map[uint64]bool
	extraSubs   []mkv.Block
	progress    mkv.ProgressFunc
}

// trackEndState tracks one output track's end while streaming: the last frame's
// timecode, the smallest positive inter-frame gap (a frame-duration estimate when
// blocks carry no explicit duration) and the largest explicit block duration.
type trackEndState struct {
	maxTC   int64
	prevTC  int64
	hasPrev bool
	minGap  int64
	maxDur  int64
}

func streamToWriter(ctx context.Context, mw *writer.MKVWriter, srcPath string, timecodeScale int64, fs *mkv.FS, opts streamOpts) error {
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, timecodeScale)
	if err != nil {
		return err
	}

	if opts.progress != nil {
		stat, _ := fs.DoStat(srcPath)
		if stat != nil {
			br.SetProgress(opts.progress, stat.Size())
		}
	}

	var cluster []mkv.Block
	clusterTS := int64(-1)
	subIdx := 0
	gateSkipped := false // keyframeAlign dropped in-range blocks waiting for a cut keyframe
	endStates := map[uint64]*trackEndState{}

	flush := func() error {
		if len(cluster) == 0 {
			return nil
		}
		err := mw.WriteClusterWithCues(clusterTS, timecodeScale, cluster)
		cluster = cluster[:0]
		return err
	}

	// injectSubs appends the pending extra subtitle blocks up to the given
	// timecode, rolling the cluster forward when a cue is too far from the
	// cluster start (a SimpleBlock offset must fit in int16 timecode units, so
	// sparse cues cannot all share one cluster).
	injectSubs := func(upTo int64) error {
		for subIdx < len(opts.extraSubs) && opts.extraSubs[subIdx].Timecode <= upTo {
			sub := opts.extraSubs[subIdx]
			if clusterTS >= 0 && sub.Timecode-clusterTS >= defaultClusterDurationMs {
				if err := flush(); err != nil {
					return err
				}
				clusterTS = sub.Timecode
			}
			if clusterTS < 0 {
				clusterTS = sub.Timecode
			}
			cluster = append(cluster, sub)
			subIdx++
		}
		return nil
	}

	// recordEnds publishes each output track's next free start (its last frame's
	// end) so a concatenating caller can rebase the following file per track.
	recordEnds := func() {
		if opts.trackEnds == nil {
			return
		}
		for id, s := range endStates {
			frame := s.maxDur
			if s.minGap > frame {
				frame = s.minGap
			}
			if frame <= 0 {
				frame = 1 // no second frame seen: nudge to avoid an exact overlap
			}
			opts.trackEnds[id] = s.maxTC + frame
		}
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			if clusterTS < 0 && gateSkipped {
				return noCutKeyframeErr(opts.timeStart)
			}
			if err := injectSubs(1 << 62); err != nil {
				return err
			}
			recordEnds()
			return flush()
		}
		if err != nil {
			return err
		}

		// cutKeyframe: a keyframe a segment may start or end on. With a video
		// track only a video keyframe qualifies (see videoTracks above).
		cutKeyframe := blk.Keyframe && (len(opts.videoTracks) == 0 || opts.videoTracks[blk.TrackNumber])

		if opts.timeStart > 0 && blk.Timecode < opts.timeStart {
			continue
		}
		// keyframeAlign: drop everything (audio included) until the first cut
		// keyframe, so the segment starts decodable and A/V start together.
		if opts.keyframeAlign && opts.timeStart > 0 && clusterTS < 0 && !cutKeyframe {
			gateSkipped = true
			continue
		}
		if opts.timeEnd > 0 && blk.Timecode >= opts.timeEnd {
			if clusterTS < 0 && gateSkipped {
				return noCutKeyframeErr(opts.timeStart)
			}
			if !opts.keyframeAlign || cutKeyframe {
				break
			}
			// keyframeAlign: the cut lands on the next cut keyframe at/after
			// timeEnd. Blocks before it belong to the GOP that started inside
			// the range: keep them so no frame is lost across segments.
		}

		newID, ok := opts.remap[blk.TrackNumber]
		if !ok {
			continue
		}
		blk.TrackNumber = newID

		off := opts.timeOffset
		if opts.trackOffsets != nil {
			off = opts.trackOffsets[newID]
		}
		blk.Timecode = blk.Timecode - opts.timeStart + off

		if opts.trackEnds != nil {
			s := endStates[newID]
			if s == nil {
				s = &trackEndState{}
				endStates[newID] = s
			}
			if s.hasPrev && blk.Timecode > s.prevTC {
				if g := blk.Timecode - s.prevTC; s.minGap == 0 || g < s.minGap {
					s.minGap = g
				}
			}
			s.prevTC, s.hasPrev = blk.Timecode, true
			if blk.Timecode > s.maxTC {
				s.maxTC = blk.Timecode
			}
			if blk.Duration > s.maxDur {
				s.maxDur = blk.Duration
			}
		}

		if clusterTS < 0 {
			clusterTS = blk.Timecode
		}
		if blk.Timecode-clusterTS >= defaultClusterDurationMs && len(cluster) > 0 {
			if err := injectSubs(blk.Timecode); err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			clusterTS = blk.Timecode
		}

		if err := injectSubs(blk.Timecode); err != nil {
			return err
		}
		cluster = append(cluster, blk)
	}

	if err := injectSubs(1 << 62); err != nil {
		return err
	}
	recordEnds()
	return flush()
}

// noCutKeyframeErr reports a range that contains blocks but no video keyframe
// to start on: writing it would either drop the content silently (empty part)
// or produce corrupt video until the next real keyframe, so it is an explicit
// error instead.
func noCutKeyframeErr(startMs int64) error {
	return fmt.Errorf("no video keyframe at/after %dms: cannot start a decodable segment there (video keyframes are sparse in the source); adjust the range to a keyframe", startMs)
}

func identityRemap(tracks []mkv.Track) map[uint64]uint64 {
	remap := make(map[uint64]uint64, len(tracks))
	for _, t := range tracks {
		remap[t.ID] = t.ID
	}
	return remap
}

// mergeSource is one input to streamMergeToWriter: a file, its TimecodeScale,
// and the map of its source track numbers to the output track numbers to keep.
type mergeSource struct {
	path  string
	scale int64
	remap map[uint64]uint64
}

// trackStats accumulates one output track's media statistics while streaming,
// for the mkvmerge-style statistics tags (BPS/DURATION/NUMBER_OF_FRAMES/…).
type trackStats struct {
	bytes   int64
	frames  int64
	minTC   int64
	maxTC   int64
	lastDur int64
	seen    bool
}

func (s *trackStats) add(b *mkv.Block) {
	s.bytes += int64(len(b.Data))
	s.frames++
	if !s.seen || b.Timecode < s.minTC {
		s.minTC = b.Timecode
	}
	if !s.seen || b.Timecode > s.maxTC {
		s.maxTC = b.Timecode
		s.lastDur = b.Duration
	}
	s.seen = true
}

// durationMs returns the track's media duration estimate: last frame start −
// first frame start, plus the last frame's explicit duration when it has one.
func (s *trackStats) durationMs() int64 {
	if !s.seen {
		return 0
	}
	return s.maxTC - s.minTC + s.lastDur
}

// streamMergeToWriter k-way merges the wanted blocks of several sources by
// timecode and writes them as time-bounded clusters. It holds only one block
// per source plus the current cluster in memory -- bounded regardless of file
// size, so it is safe for very large inputs (unlike collecting + sorting every
// block). Block.Timecode is in milliseconds for every source, so cross-source
// comparison needs no scale normalisation. The returned map holds each output
// track's accumulated statistics (keyed by output track number).
func streamMergeToWriter(ctx context.Context, mw *writer.MKVWriter, outScale int64, fs *mkv.FS, sources []mergeSource, progress mkv.ProgressFunc) (map[uint64]*trackStats, error) {
	type state struct {
		br   *reader.BlockReader
		f    mkv.ReadSeekCloser
		head mkv.Block
		ok   bool
	}
	states := make([]*state, len(sources))
	defer func() {
		for _, s := range states {
			if s != nil && s.f != nil {
				s.f.Close()
			}
		}
	}()

	// Aggregated progress: each source reports its own byte position; the
	// caller sees the sum over the total size of all sources.
	var progTotal int64
	var progPositions []int64
	if progress != nil {
		progPositions = make([]int64, len(sources))
		for _, src := range sources {
			if st, _ := fs.DoStat(src.path); st != nil {
				progTotal += st.Size()
			}
		}
	}

	advance := func(i int) error {
		st := states[i]
		for {
			blk, err := st.br.Next()
			if err == io.EOF {
				st.ok = false
				return nil
			}
			if err != nil {
				return err
			}
			newID, ok := sources[i].remap[blk.TrackNumber]
			if !ok {
				continue
			}
			blk.TrackNumber = newID
			st.head, st.ok = blk, true
			return nil
		}
	}

	for i, src := range sources {
		f, err := fs.DoOpen(src.path)
		if err != nil {
			return nil, err
		}
		br, err := reader.NewBlockReader(f, src.scale)
		if err != nil {
			f.Close()
			return nil, err
		}
		if progress != nil {
			idx := i
			br.SetProgress(func(p, _ int64) {
				progPositions[idx] = p
				var sum int64
				for _, v := range progPositions {
					sum += v
				}
				progress(sum, progTotal)
			}, progTotal)
		}
		states[i] = &state{br: br, f: f}
		if err := advance(i); err != nil {
			return nil, err
		}
	}

	var cluster []mkv.Block
	clusterTS := int64(-1)
	flush := func() error {
		if len(cluster) == 0 {
			return nil
		}
		err := mw.WriteClusterWithCues(clusterTS, outScale, cluster)
		cluster = cluster[:0]
		return err
	}

	stats := make(map[uint64]*trackStats)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Pick the source whose head block has the smallest timecode. Each
		// source is read in file order; for the common single-track-per-source
		// case that order is already sorted, so this reproduces a global sort.
		mi := -1
		for i, st := range states {
			if !st.ok {
				continue
			}
			if mi < 0 || st.head.Timecode < states[mi].head.Timecode {
				mi = i
			}
		}
		if mi < 0 {
			break
		}
		blk := states[mi].head
		if err := advance(mi); err != nil {
			return nil, err
		}
		ts := stats[blk.TrackNumber]
		if ts == nil {
			ts = &trackStats{}
			stats[blk.TrackNumber] = ts
		}
		ts.add(&blk)

		if clusterTS < 0 {
			clusterTS = blk.Timecode
		}
		if blk.Timecode-clusterTS >= defaultClusterDurationMs && len(cluster) > 0 {
			if err := flush(); err != nil {
				return nil, err
			}
			clusterTS = blk.Timecode
		}
		cluster = append(cluster, blk)
	}
	return stats, flush()
}
