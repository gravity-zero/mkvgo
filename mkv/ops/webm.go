package ops

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// RemuxToWebM reads srcPath and writes a complete, playable, SEEKABLE WebM
// file to dstPath: it validates that every track uses a WebM-compatible codec,
// then copies the media (every block, verbatim) into a "webm"-DocType container
// with time-bounded clusters, a Cues seek index and a SeekHead. It does NOT
// transcode - a source whose codecs fall outside the WebM subset is rejected
// with an error and no output is produced.
//
// Elements NOT carried into the output: Chapters, Attachments and Tags are
// dropped (mkv.WebMNonSubsetElements lists the ones present in a source).
//
// Unlike writer.WriteWebM (metadata only), this produces a file with frames.
func RemuxToWebM(ctx context.Context, srcPath, dstPath string, extra ...mkv.Options) (err error) {
	fs := mkv.FSFrom(extra)

	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	if err := mkv.ValidateWebM(c); err != nil {
		return err
	}

	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	br, err := reader.NewBlockReader(src, c.Info.TimecodeScale)
	if err != nil {
		return err
	}
	if p := mkv.ProgressFrom(extra); p != nil {
		if st, _ := fs.DoStat(srcPath); st != nil {
			br.SetProgress(p, st.Size())
		}
	}

	dst, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(dst, &err)

	mw := writer.NewMKVWriter(dst)
	if err := mw.WriteStartWebM(mkv.WebMDocTypeVersion(c)); err != nil {
		return err
	}
	// Only Info + Tracks: chapters/attachments/tags are outside the WebM subset,
	// and so is the segment identity itself - the WebM element table lists
	// SegmentUID, PrevUID and NextUID as Unsupported, so the output carries
	// none rather than a derived one.
	meta := *c
	meta.Chapters, meta.Attachments, meta.Tags = nil, nil, nil
	meta.Info.SegmentUID, meta.Info.PrevUID, meta.Info.NextUID = nil, nil, nil
	if err := mw.WriteMetadata(&meta, c.Tracks, c.DurationMs); err != nil {
		return err
	}

	// Group blocks into time-bounded clusters (~1s) rather than splitting on
	// every keyframe: in multiplexed A/V every Opus frame is a "keyframe", so a
	// keyframe-per-cluster policy would emit one tiny cluster per audio frame.
	var cluster []mkv.Block
	clusterStart := int64(-1)
	flush := func() error {
		if len(cluster) == 0 {
			return nil
		}
		err := mw.WriteClusterWithCues(clusterStart, c.Info.TimecodeScale, cluster)
		cluster = cluster[:0]
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("remux webm: read block: %w", err)
		}
		if clusterStart >= 0 && b.Timecode-clusterStart >= defaultClusterDurationMs {
			if err := flush(); err != nil {
				return fmt.Errorf("remux webm: write cluster: %w", err)
			}
			clusterStart = -1
		}
		if clusterStart < 0 {
			clusterStart = b.Timecode
		}
		cluster = append(cluster, b)
	}
	if err := flush(); err != nil {
		return fmt.Errorf("remux webm: write cluster: %w", err)
	}
	return mw.Finalize()
}
