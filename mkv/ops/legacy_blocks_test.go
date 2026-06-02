package ops

// Test-only block helpers. Production code uses streamToWriter /
// streamMergeToWriter (stream.go); these load all blocks in memory and are kept
// only because their unit tests still exercise the cluster-batching, merge and
// filter logic directly.

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func writeBlocksAsClusters(mw *writer.MKVWriter, blocks []mkv.Block, timecodeScale int64) error {
	if len(blocks) == 0 {
		return nil
	}
	var cluster []mkv.Block
	clusterTS := blocks[0].Timecode

	for i := range blocks {
		b := &blocks[i]
		if b.Timecode-clusterTS >= defaultClusterDurationMs && len(cluster) > 0 {
			if err := mw.WriteClusterWithCues(clusterTS, timecodeScale, cluster); err != nil {
				return err
			}
			cluster = cluster[:0]
			clusterTS = b.Timecode
		}
		cluster = append(cluster, *b)
	}
	if len(cluster) > 0 {
		return mw.WriteClusterWithCues(clusterTS, timecodeScale, cluster)
	}
	return nil
}

func readFilteredBlocks(ctx context.Context, path string, timecodeScale int64, remap map[uint64]uint64, fs *mkv.FS) ([]mkv.Block, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, timecodeScale)
	if err != nil {
		return nil, err
	}

	var blocks []mkv.Block
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		newID, ok := remap[blk.TrackNumber]
		if !ok {
			continue
		}
		blk.TrackNumber = newID
		blocks = append(blocks, blk)
	}
	return blocks, nil
}

func mergeBlocks(a, b []mkv.Block) []mkv.Block {
	merged := make([]mkv.Block, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Timecode <= b[j].Timecode {
			merged = append(merged, a[i])
			i++
		} else {
			merged = append(merged, b[j])
			j++
		}
	}
	merged = append(merged, a[i:]...)
	merged = append(merged, b[j:]...)
	return merged
}
