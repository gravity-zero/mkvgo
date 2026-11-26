package ops

import (
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/internal/dovi"
	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// ConvertDolbyVision writes dstPath: srcPath with the layers of its HEVC Dolby Vision tracks converted as conv says,
// no picture re-encoded and every base layer byte for byte. It refuses a source no track of which conv rewrites.
func ConvertDolbyVision(ctx context.Context, srcPath, dstPath string, conv mkv.DolbyVisionConversion, opts ...mkv.Options) (err error) {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	tracks, rewriters, err := doviTracks(c.Tracks, conv)
	if err != nil {
		return err
	}
	if len(rewriters) == 0 {
		return fmt.Errorf("%s: no track to convert: a Dolby Vision profile 7 HEVC track is needed to reach profile 8.1, a profile 7 or 8 one to keep the base layer alone", shown.Path(srcPath))
	}

	out, err := createOutput(fs, mkv.SyncOutputFrom(opts), dstPath, append([]string{srcPath}, attachmentPaths(c.Attachments)...)...)
	if err != nil {
		return err
	}
	defer out.finish(&err)

	mw := writer.NewMKVWriter(out)
	mw.SetAttachmentSource(attachmentSource(fs))
	if err := writeStartLike(mw, c, tracks); err != nil {
		return err
	}
	meta := *c
	meta.Info.SegmentUID = derivedSegmentUID(&c.Info, srcPath, "dolby-vision")
	plan := planContentTags(c.Tags).withStatistics() // the sizes change: measured as the blocks go by
	meta.Tags = nil
	digests, stats := plan.digestsFor(), plan.statsFor()
	if err := mw.WriteMetadata(&meta, tracks, c.DurationMs); err != nil {
		return err
	}
	if err := mw.ReserveTags(plan.upperBoundTags(tracks)); err != nil {
		return err
	}
	if err := streamToWriter(ctx, mw, srcPath, c.Info.TimecodeScale, fs, streamOpts{
		remap: identityRemap(c.Tracks), dovi: rewriters, contentDigests: digests, contentStats: stats,
		progress: mkv.ProgressFrom(opts),
	}); err != nil {
		return err
	}
	if err := mw.WriteReservedTags(plan.tagsForOutput(tracks, digests, stats)); err != nil {
		return err
	}
	return mw.Finalize()
}

// doviTracks returns tracks as conv leaves them and, per track number, the rewriter of each track conv converts.
func doviTracks(src []mkv.Track, conv mkv.DolbyVisionConversion) ([]mkv.Track, map[uint64]*dovi.Track, error) {
	tracks := append([]mkv.Track(nil), src...)
	rewriters := map[uint64]*dovi.Track{}
	for i := range tracks {
		k, err := dovi.For(&tracks[i], conv)
		if err != nil {
			return nil, nil, err
		}
		if k != nil {
			rewriters[tracks[i].ID] = k
		}
	}
	return tracks, rewriters, nil
}
