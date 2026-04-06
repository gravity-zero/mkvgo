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
