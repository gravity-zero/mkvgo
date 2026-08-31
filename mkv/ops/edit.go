package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func RemoveTrack(ctx context.Context, srcPath, dstPath string, removeIDs []uint64, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
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

	// Tags targeting a removed track's UID would be orphans in the output:
	// drop them. Global tags (TargetID 0) and tags on kept tracks survive.
	removedUIDs := map[uint64]bool{}
	for _, t := range c.Tracks {
		if remove[t.ID] {
			uid := t.UID
			if uid == 0 {
				uid = t.ID // the writer defaults a zero UID to the track ID
			}
			removedUIDs[uid] = true
		}
	}
	if len(c.Tags) > 0 {
		keptTags := c.Tags[:0]
		for _, tag := range c.Tags {
			if tag.TargetID != 0 && removedUIDs[tag.TargetID] {
				continue
			}
			keptTags = append(keptTags, tag)
		}
		c.Tags = keptTags
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := mw.WriteStart(); err != nil {
		return err
	}
	// A file with fewer tracks is not the file it came from: it gets its own
	// derived identity instead of the source's (see derivedSegmentUID).
	meta := *c
	meta.Info.SegmentUID = derivedSegmentUID(&c.Info, srcPath, "remove-track")
	if err := mw.WriteMetadata(&meta, kept, c.DurationMs); err != nil {
		return err
	}
	if err := streamToWriter(ctx, mw, srcPath, c.Info.TimecodeScale, fs, streamOpts{
		remap: remap, progress: mkv.ProgressFrom(opts),
	}); err != nil {
		return err
	}
	return mw.Finalize()
}

func AddTrack(ctx context.Context, srcPath, dstPath string, input mkv.TrackInput, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}

	srcAdd, err := reader.OpenWithFS(ctx, input.SourcePath, fs, reader.WithoutAttachmentData())
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

	// An added track that outlasts the source makes the output longer than the
	// source's Info declares, so that declaration has to go (when it does not,
	// keeping it preserves its sub-millisecond precision).
	meta := *c
	durationMs := c.DurationMs
	if srcAdd.DurationMs > durationMs {
		durationMs = srcAdd.DurationMs
		meta = metaForNewDuration(c)
	}
	// A file with one more track is not the file it came from (derivedSegmentUID).
	meta.Info.SegmentUID = derivedSegmentUID(&c.Info, srcPath, "add-track")

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := mw.WriteStart(); err != nil {
		return err
	}
	if err := mw.WriteMetadata(&meta, tracks, durationMs); err != nil {
		return err
	}
	sources := []mergeSource{
		{path: srcPath, scale: c.Info.TimecodeScale, remap: remap},
		{path: input.SourcePath, scale: srcAdd.Info.TimecodeScale, remap: map[uint64]uint64{input.TrackID: newID}},
	}
	if _, err := streamMergeToWriter(ctx, mw, c.Info.TimecodeScale, fs, sources, mkv.ProgressFrom(opts)); err != nil {
		return err
	}
	return mw.Finalize()
}

func EditMetadata(ctx context.Context, srcPath, dstPath string, edit func(*mkv.Container), opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}

	edit(c)
	syncDurationMs(c)

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
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
	fastErr := reindexFastCopy(mw, srcPath, c.Info.TimecodeScale, fs, progress, totalBytes, videoTrackSet(c.Tracks))
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
