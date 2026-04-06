package ops

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func RemoveTrack(ctx context.Context, srcPath, dstPath string, removeIDs []uint64, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	remove := make(map[uint64]bool, len(removeIDs))
	for _, id := range removeIDs {
		remove[id] = true
	}

	var kept []mkv.Track
	remap := map[uint64]uint64{}
	newID := uint64(1)
	for _, t := range c.Tracks {
		if remove[t.ID] {
			continue
		}
		remap[t.ID] = newID
		t.ID = newID
		kept = append(kept, t)
		newID++
	}
	if len(kept) == 0 {
		return fmt.Errorf("cannot remove all tracks")
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	if err := mw.WriteMetadata(c, kept, c.DurationMs); err != nil {
		return err
	}
	if err := streamToWriter(ctx, mw, srcPath, c.Info.TimecodeScale, fs, streamOpts{
		remap: remap, progress: mkv.ProgressFrom(opts),
	}); err != nil {
		return err
	}
	return mw.Finalize()
}

func AddTrack(ctx context.Context, srcPath, dstPath string, input mkv.TrackInput, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	srcAdd, err := reader.OpenWithFS(ctx, input.SourcePath, fs)
	if err != nil {
		return err
	}
	var addedTrack *mkv.Track
	for i := range srcAdd.Tracks {
		if srcAdd.Tracks[i].ID == input.TrackID {
			addedTrack = &srcAdd.Tracks[i]
			break
		}
	}
	if addedTrack == nil {
		return fmt.Errorf("track %d not found in %s", input.TrackID, input.SourcePath)
	}

	newID := uint64(len(c.Tracks) + 1)
	remap := identityRemap(c.Tracks)

	blocks, err := readFilteredBlocks(ctx, srcPath, c.Info.TimecodeScale, remap, fs)
	if err != nil {
		return err
	}

	addRemap := map[uint64]uint64{input.TrackID: newID}
	addBlocks, err := readFilteredBlocks(ctx, input.SourcePath, srcAdd.Info.TimecodeScale, addRemap, fs)
	if err != nil {
		return err
	}
	blocks = mergeBlocks(blocks, addBlocks)

	t := *addedTrack
	t.ID = newID
	if input.Language != "" {
		t.Language = input.Language
	}
	if input.Name != "" {
		t.Name = input.Name
	}
	t.IsDefault = input.IsDefault
	tracks := append(c.Tracks, t)

	var durationMs int64
	if len(blocks) > 0 {
		durationMs = blocks[len(blocks)-1].Timecode
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		return err
	}
	if err := writeBlocksAsClusters(mw, blocks, c.Info.TimecodeScale); err != nil {
		return err
	}
	return mw.Finalize()
}

func EditMetadata(ctx context.Context, srcPath, dstPath string, edit func(*mkv.Container), opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}

	edit(c)

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	if err := mw.WriteMetadata(c, c.Tracks, c.DurationMs); err != nil {
		return err
	}
	if err := streamToWriter(ctx, mw, srcPath, c.Info.TimecodeScale, fs, streamOpts{
		remap: identityRemap(c.Tracks), progress: mkv.ProgressFrom(opts),
	}); err != nil {
		return err
	}
	return mw.Finalize()
}

func ExtractAttachment(ctx context.Context, srcPath string, attachID uint64, outPath string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}
	for _, a := range c.Attachments {
		if a.ID == attachID {
			return fs.DoWriteFile(outPath, a.Data, 0644)
		}
	}
	return fmt.Errorf("attachment %d not found", attachID)
}

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
