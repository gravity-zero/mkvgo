package ops

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

func Demux(ctx context.Context, opts mkv.DemuxOptions, extra ...mkv.Options) error {
	fs := mkv.FSFrom(extra)
	c, err := reader.OpenWithFS(ctx, opts.SourcePath, fs)
	if err != nil {
		return err
	}

	wanted := buildTrackSet(c, opts.TrackIDs)
	if len(wanted) == 0 {
		return fmt.Errorf("no matching tracks found")
	}

	if err := fs.DoMkdirAll(opts.OutputDir, 0755); err != nil {
		return err
	}

	writers, closers, err := openOutputFiles(wanted, opts.OutputDir, fs)
	if err != nil {
		return err
	}
	defer func() {
		for _, cl := range closers {
			cl.Close()
		}
	}()

	f, err := fs.DoOpen(opts.SourcePath)
	if err != nil {
		return err
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return err
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read block: %w", err)
		}
		track, ok := wanted[blk.TrackNumber]
		if !ok {
			continue
		}
		w := writers[blk.TrackNumber]
		data := track.RestoreHeader(blk.Data)
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write track %d: %w", blk.TrackNumber, err)
		}
	}
	return nil
}

func buildTrackSet(c *mkv.Container, trackIDs []uint64) map[uint64]mkv.Track {
	m := make(map[uint64]mkv.Track)
	if len(trackIDs) == 0 {
		for _, t := range c.Tracks {
			m[t.ID] = t
		}
		return m
	}
	idx := make(map[uint64]mkv.Track, len(c.Tracks))
	for _, t := range c.Tracks {
		idx[t.ID] = t
	}
	for _, id := range trackIDs {
		if t, ok := idx[id]; ok {
			m[id] = t
		}
	}
	return m
}

func openOutputFiles(tracks map[uint64]mkv.Track, dir string, fs *mkv.FS) (map[uint64]io.Writer, []io.Closer, error) {
	writers := make(map[uint64]io.Writer, len(tracks))
	var closers []io.Closer
	for id, t := range tracks {
		ext := sanitizeCodec(t.Codec)
		name := fmt.Sprintf("%d.%s", id, ext)
		path, err := safePath(dir, name)
		if err != nil {
			for _, cl := range closers {
				cl.Close()
			}
			return nil, nil, err
		}
		f, err := fs.DoCreate(path)
		if err != nil {
			for _, cl := range closers {
				cl.Close()
			}
			return nil, nil, err
		}
		writers[id] = f
		closers = append(closers, f)
	}
	return writers, closers, nil
}
