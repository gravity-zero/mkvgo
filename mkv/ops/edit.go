package ops

import (
	"context"
	"fmt"

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

	durationMs := c.DurationMs
	if srcAdd.DurationMs > durationMs {
		durationMs = srcAdd.DurationMs
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
	sources := []mergeSource{
		{path: srcPath, scale: c.Info.TimecodeScale, remap: remap},
		{path: input.SourcePath, scale: srcAdd.Info.TimecodeScale, remap: map[uint64]uint64{input.TrackID: newID}},
	}
	if err := streamMergeToWriter(ctx, mw, c.Info.TimecodeScale, fs, sources); err != nil {
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

	progress := mkv.ProgressFrom(opts)

	// Fast path: copy clusters verbatim and rebuild only the Cues index.
	// Falls back on unknown-size clusters (streaming files) only.
	// When a progress callback is provided, reindexFastCopy reports progress
	// per cluster using the source file size as the total.
	var totalBytes int64
	if progress != nil {
		if stat, _ := fs.DoStat(srcPath); stat != nil {
			totalBytes = stat.Size()
		}
	}
	fastErr := reindexFastCopy(mw, srcPath, c.Info.TimecodeScale, fs, progress, totalBytes)
	if fastErr == nil {
		return mw.Finalize()
	}
	if fastErr != errUnknownSizeCluster {
		return fastErr
	}
	// errUnknownSizeCluster: fall through to streamToWriter.

	if err := streamToWriter(ctx, mw, srcPath, c.Info.TimecodeScale, fs, streamOpts{
		remap: identityRemap(c.Tracks), progress: progress,
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
