package ops

import (
	"bytes"
	"context"
	"fmt"
	"io"

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
		// The attachment payloads stay on disk: Join only forwards them, and a
		// font set held for the whole operation is what a small container cannot
		// afford. They are streamed straight from the source at write time.
		c, err := reader.OpenWithFS(ctx, src, fs, reader.WithoutAttachmentData())
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
	//
	// Chapters come from EVERY source, not just the first: they are timeline
	// data, and keeping only the first file's would silently return one chapter
	// where twelve were joined. They cannot be written yet though - each one
	// needs the offset its own source ends up at - so the head only books the
	// room here (sized on the list, which is already known) and the timestamps
	// are filled in once the last block has been written.
	//
	// The content hash and statistics of the first file describe the first file
	// only; the joined one measures its own while streaming and states them
	// after the clusters.
	meta := metaForNewDuration(first)
	meta.Chapters = nil
	// Attachments are pooled, not taken from the first file alone: the fonts an
	// ASS track needs may only be attached to the part that uses them, and a
	// first-wins policy drops them. Matched on name + size, so the same font
	// attached to every part lands once.
	meta.Attachments = mergeAttachments(conts, fs)
	plan := planContentTags(first.Tags).withStatistics()
	meta.Tags = nil // booked below, in the head, filled once measured
	digests, stats := plan.digestsFor(), plan.statsFor()
	// The payloads are still on disk: hand the writer the way to reach them so
	// the element is written from its original position, byte for byte, without
	// a font ever becoming resident.
	mw.SetAttachmentSource(func(a *mkv.Attachment) (io.Reader, error) {
		f, err := fs.DoOpen(a.DataPath)
		if err != nil {
			return nil, err
		}
		if _, err := f.Seek(a.DataOffset, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	})
	if err := mw.WriteMetadata(&meta, first.Tracks, totalDurationMs); err != nil {
		return err
	}
	if err := mw.ReserveChapters(concatChapters(conts, nil)); err != nil {
		return err
	}
	if err := mw.ReserveTags(plan.upperBoundTags(first.Tracks)); err != nil {
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

	// ONE offset shared by every track: a file is appended after the end of the
	// whole previous file, so all its tracks shift by the same amount and keep
	// the alignment they had at the source.
	//
	// Rebasing each track on its own end instead (what this did) slides a track
	// forward relative to the others by however much earlier it happened to
	// stop: a subtitle track whose last cue is minutes before the end of a part
	// comes back minutes early in the next one. On a real episode split at
	// 10:00 that is 5m50s of desync, and the block then lands so far outside its
	// cluster that the write fails on SimpleBlock's int16 relative timecode.
	//
	// The offset is the LAST FRAME'S END across the tracks, not the declared
	// duration: a container that declares more than it holds must not open a
	// hole at the seam.
	var offset int64
	offsets := make([]int64, len(sources)) // where each source's timeline starts
	for i, src := range sources {
		offsets[i] = offset
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
			remap: remap, timeOffset: offset, trackEnds: trackEnds,
			outScale:       first.Info.TimecodeScale,
			contentDigests: digests, contentStats: stats,
			progress: srcProgress,
		}); err != nil {
			return fmt.Errorf("join %s: %w", src, err)
		}
		for _, end := range trackEnds {
			if end > offset {
				offset = end
			}
		}
	}

	// What was actually written runs to the last seam offset, which is more than
	// the sum of the sources' DECLARED durations whenever they were cut on
	// keyframes (each part keeps the GOP straddling its end). Left as declared,
	// a rejoined film's seek bar stops short - 24.8 s short on a 12-part split.
	if err := mw.RestateDuration(offset); err != nil {
		return err
	}

	// The seam offsets are known now: same values the blocks were written with.
	if err := mw.WriteReservedChapters(concatChapters(conts, offsets)); err != nil {
		return err
	}
	if err := mw.WriteReservedTags(plan.tagsForOutput(first.Tracks, digests, stats)); err != nil {
		return err
	}
	return mw.Finalize()
}
