package ops

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

const defaultClusterDurationMs = 1000

type streamOpts struct {
	remap         map[uint64]uint64
	timeOffset    int64
	timeStart     int64
	timeEnd       int64
	keyframeAlign bool // split on keyframe boundaries
	extraSubs     []mkv.Block
	progress      mkv.ProgressFunc
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

	flush := func() error {
		if len(cluster) == 0 {
			return nil
		}
		err := mw.WriteClusterWithCues(clusterTS, timecodeScale, cluster)
		cluster = cluster[:0]
		return err
	}

	injectSubs := func(upTo int64) {
		for subIdx < len(opts.extraSubs) && opts.extraSubs[subIdx].Timecode <= upTo {
			cluster = append(cluster, opts.extraSubs[subIdx])
			subIdx++
		}
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			injectSubs(1 << 62)
			return flush()
		}
		if err != nil {
			return err
		}

		if opts.timeStart > 0 && blk.Timecode < opts.timeStart {
			continue
		}
		// keyframeAlign: wait for a keyframe to actually start writing
		if opts.keyframeAlign && opts.timeStart > 0 && clusterTS < 0 && !blk.Keyframe {
			continue
		}
		if opts.timeEnd > 0 && blk.Timecode >= opts.timeEnd {
			// keyframeAlign: keep going until a keyframe for clean cut
			if opts.keyframeAlign && !blk.Keyframe {
				continue // skip non-keyframes past the end point
			}
			break
		}

		newID, ok := opts.remap[blk.TrackNumber]
		if !ok {
			continue
		}
		blk.TrackNumber = newID

		blk.Timecode = blk.Timecode - opts.timeStart + opts.timeOffset

		if clusterTS < 0 {
			clusterTS = blk.Timecode
		}
		if blk.Timecode-clusterTS >= defaultClusterDurationMs && len(cluster) > 0 {
			injectSubs(blk.Timecode)
			if err := flush(); err != nil {
				return err
			}
			clusterTS = blk.Timecode
		}

		injectSubs(blk.Timecode)
		cluster = append(cluster, blk)
	}

	injectSubs(1 << 62)
	return flush()
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

// streamMergeToWriter k-way merges the wanted blocks of several sources by
// timecode and writes them as time-bounded clusters. It holds only one block
// per source plus the current cluster in memory -- bounded regardless of file
// size, so it is safe for very large inputs (unlike collecting + sorting every
// block). Block.Timecode is in milliseconds for every source, so cross-source
// comparison needs no scale normalisation.
func streamMergeToWriter(ctx context.Context, mw *writer.MKVWriter, outScale int64, fs *mkv.FS, sources []mergeSource) error {
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
			return err
		}
		br, err := reader.NewBlockReader(f, src.scale)
		if err != nil {
			f.Close()
			return err
		}
		states[i] = &state{br: br, f: f}
		if err := advance(i); err != nil {
			return err
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

	for {
		if ctx.Err() != nil {
			return ctx.Err()
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
			return err
		}

		if clusterTS < 0 {
			clusterTS = blk.Timecode
		}
		if blk.Timecode-clusterTS >= defaultClusterDurationMs && len(cluster) > 0 {
			if err := flush(); err != nil {
				return err
			}
			clusterTS = blk.Timecode
		}
		cluster = append(cluster, blk)
	}
	return flush()
}
