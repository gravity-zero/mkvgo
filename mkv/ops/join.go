package ops

import (
	"bytes"
	"context"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Join(ctx context.Context, sources []string, dstPath string, opts ...mkv.Options) (err error) {
	fs := mkv.FSFrom(opts)
	if len(sources) == 0 {
		return fmt.Errorf("no sources to join")
	}

	// Open each source once for its metadata (it was opened three times before).
	conts := make([]*mkv.Container, len(sources))
	for i, src := range sources {
		c, err := reader.OpenWithFS(ctx, src, fs)
		if err != nil {
			return err
		}
		conts[i] = c
	}
	first := conts[0]

	// Every source must line up with the first: same track count, types AND codecs
	// (a codec mismatch - e.g. H.264 + HEVC - would silently produce a broken file).
	var totalDurationMs int64
	for i, c := range conts {
		if i > 0 {
			if len(c.Tracks) != len(first.Tracks) {
				return fmt.Errorf("%s has %d tracks, expected %d", sources[i], len(c.Tracks), len(first.Tracks))
			}
			for j, t := range c.Tracks {
				ft := first.Tracks[j]
				switch {
				case t.Type != ft.Type:
					return fmt.Errorf("%s track %d: type %s, expected %s", sources[i], j+1, t.Type, ft.Type)
				case t.Codec != ft.Codec:
					return fmt.Errorf("%s track %d: codec %s, expected %s (cannot concatenate)", sources[i], j+1, t.Codec, ft.Codec)
				case !bytes.Equal(t.CodecPrivate, ft.CodecPrivate):
					return fmt.Errorf("%s track %d: codec configuration differs from the first file (cannot concatenate)", sources[i], j+1)
				}
			}
		}
		totalDurationMs += c.DurationMs
	}

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return err
	}
	defer closeWithErr(out, &err)

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	// The joined file is as long as its sources put together, not as long as
	// the first one - whose Info the metadata is otherwise copied from.
	meta := metaForNewDuration(first)
	if err := mw.WriteMetadata(&meta, first.Tracks, totalDurationMs); err != nil {
		return err
	}

	// Aggregated progress across the sources (bytes processed / total bytes).
	progress := mkv.ProgressFrom(opts)
	var progDone, progTotal int64
	if progress != nil {
		for _, src := range sources {
			if st, _ := fs.DoStat(src); st != nil {
				progTotal += st.Size()
			}
		}
	}

	// Per-track offsets: each track is concatenated against ITS OWN end, not the
	// single container duration, so tracks that end at slightly different times do
	// not accumulate A/V drift across joins.
	trackOffsets := make(map[uint64]int64, len(first.Tracks))
	for i, src := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c := conts[i]
		remap := make(map[uint64]uint64, len(c.Tracks))
		for j, t := range c.Tracks {
			remap[t.ID] = first.Tracks[j].ID
		}
		var srcProgress mkv.ProgressFunc
		if progress != nil {
			done := progDone
			srcProgress = func(p, _ int64) { progress(done+p, progTotal) }
			if st, _ := fs.DoStat(src); st != nil {
				progDone += st.Size()
			}
		}
		trackEnds := make(map[uint64]int64, len(c.Tracks))
		if err := streamToWriter(ctx, mw, src, c.Info.TimecodeScale, fs, streamOpts{
			remap: remap, trackOffsets: trackOffsets, trackEnds: trackEnds,
			progress: srcProgress,
		}); err != nil {
			return fmt.Errorf("join %s: %w", src, err)
		}
		for id, end := range trackEnds {
			if end > trackOffsets[id] {
				trackOffsets[id] = end
			}
		}
	}

	return mw.Finalize()
}
