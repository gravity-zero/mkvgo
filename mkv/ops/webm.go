package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// RemuxToWebM reads srcPath and writes a complete, playable WebM file to
// dstPath: it validates that every track uses a WebM-compatible codec, then
// copies the media (every block, verbatim) into a "webm"-DocType container with
// keyframe-aligned clusters. It does NOT transcode — a source whose codecs fall
// outside the WebM subset is rejected with an error and no output is produced.
//
// Unlike writer.WriteWebM (metadata only), this produces a file with frames.
func RemuxToWebM(ctx context.Context, srcPath, dstPath string, extra ...mkv.Options) (err error) {
	fs := mkv.FSFrom(extra)

	c, err := reader.OpenWithFS(ctx, srcPath, fs)
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

	dst, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(dst, &err)

	// Buffer the output: the StreamWriter emits several small writes per block,
	// which would otherwise be a syscall each on this file-copy hot path. (The
	// StreamWriter itself stays unbuffered so live-streaming callers keep low latency.)
	buf := bufio.NewWriterSize(dst, 256<<10)
	sw, err := writer.NewWebMStreamWriter(buf, c.Info, c.Tracks)
	if err != nil {
		return err
	}

	// Group blocks into time-bounded clusters (~1s) rather than splitting on
	// every keyframe: in multiplexed A/V every Opus frame is a "keyframe", so a
	// keyframe-per-cluster policy would emit one tiny cluster per audio frame.
	const clusterDurationMs = 1000
	clusterStart := int64(-1)
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
		if clusterStart < 0 || b.Timecode-clusterStart >= clusterDurationMs {
			sw.FlushCluster() // force the next write to open a fresh cluster
			clusterStart = b.Timecode
		}
		if err := sw.WriteBlockInCurrentCluster(b); err != nil {
			return fmt.Errorf("remux webm: write block: %w", err)
		}
	}
	if err := buf.Flush(); err != nil {
		return fmt.Errorf("remux webm: flush: %w", err)
	}
	return nil
}
