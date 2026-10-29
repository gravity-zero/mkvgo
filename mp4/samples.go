package mp4

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
)

// Sample is one sample as the movie header's sample table states it: no media is read.
type Sample struct {
	// DtsMs is the decode time, PtsMs the presentation time with the edit list applied (as Container.Keyframes).
	DtsMs, PtsMs, DurMs int64
	Size                int64
	Sync                bool
}

// ForEachSample reads path's sample tables, a fragmented file's from its
// fragments, and calls fn with every sample, track by track (t is the
// returned Container's), each track's in decode order. It never reads
// a sample's bytes. headerMs is the duration the movie header (mvhd) declares,
// 0 for a fragmented file, whose fragments state their own. An error from fn
// stops the walk and is returned.
func ForEachSample(ctx context.Context, path string, fn func(t *mkv.Track, s Sample) error, opts ...Options) (c *mkv.Container, headerMs int64, err error) {
	if err := oneopt.Check(opts); err != nil {
		return nil, 0, err
	}
	o := optionsFrom(opts)
	src, err := o.FS.DoOpen(path)
	if err != nil {
		return nil, 0, err
	}
	defer src.Close()
	size, err := src.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	mv, err := parseMP4Limit(ctxReadSeeker{src, ctx}, size, sampleFull, o.moovLimit())
	if err != nil {
		return nil, 0, err
	}
	if !mv.fragmented {
		headerMs = mv.durationMs
	}
	c = containerFromMovie(mv)
	c.Path = path
	for i := range mv.tracks {
		for _, s := range mv.tracks[i].samples {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			if err := fn(&c.Tracks[i], Sample{DtsMs: s.dtsMs, PtsMs: s.ctsMs, DurMs: s.durMs, Size: int64(s.size), Sync: s.sync}); err != nil {
				return nil, 0, err
			}
		}
	}
	return c, headerMs, nil
}

// ForEachBlock hands fn every block RemuxFromMP4 would write for path, in the same order and with the same bytes,
// without writing anything: what a digest of the remux would read, read straight from the source, one sample held
// at a time. It refuses what RemuxFromMP4 refuses. The returned Container is the remux's metadata; fn's
// TrackNumber is a Container track ID.
func ForEachBlock(ctx context.Context, path string, fn func(b mkv.Block) error, opts ...Options) (*mkv.Container, error) {
	if err := oneopt.Check(opts); err != nil {
		return nil, err
	}
	o := optionsFrom(opts)
	src, err := o.FS.DoOpen(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	fi, err := o.FS.DoStat(path)
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	mv, err := parseMP4Limit(src, size, sampleFull, o.moovLimit())
	if err != nil {
		return nil, err
	}
	for _, d := range mv.dropped {
		o.report(d)
	}
	cut, isCut := mv.truncation(size)
	if err := carriableTracks(mv, &o); err != nil {
		return nil, err
	}
	if sampleCount(mv) == 0 {
		return nil, errf("the source holds no samples (an initialisation segment?): nothing to read")
	}
	c := containerFromMovie(mv)
	c.Tracks = buildMKVTracks(mv, o.MP3ContainerDelay)
	c.Path = path
	if err := forEachBlock(ctx, src, mv, size, o.Progress, func(b mkv.Block, _ int64) error { return fn(b) }); err != nil {
		return nil, err
	}
	if isCut && o.OnTruncated != nil {
		o.OnTruncated(cut)
	}
	return c, nil
}
