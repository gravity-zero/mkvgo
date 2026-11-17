package ops

// subblocks.go - the one read every subtitle extractor is built on: the blocks of one subtitle track, in file
// order, by walking the file or by seeking where a SubtitleIndex says they are. What a consumer makes of a block
// (text, a picture) is its own; where the block is, whether the index still describes the file, and what a damaged
// region costs are decided here, once.

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// SubtitleBlock is one block of a subtitle track: its time, the duration the muxer wrote for it (0 for none) and
// its payload as the codec reads it, a track stored compressed already decoded. Data is valid until the callback
// returns.
//
// A call with Lost set carries no block: the read passed over a damaged region that starts at TimeMs (-1 when the
// region has no time of its own), the blocks in it being lost. A decoder that keeps state between blocks resets on
// it, and a cue still on screen ends there.
type SubtitleBlock struct {
	TimeMs     int64
	DurationMs int64
	Data       []byte
	Additions  []mkv.BlockAddition
	Lost       bool
}

// ForEachSubtitleBlock walks srcPath once and calls fn with each block of the subtitle track trackID, whatever its
// codec, in file order. A damaged region within Options.DamageBytes is passed over, reported through OnSkip and to
// fn as a Lost call; a larger one fails the read. An error from fn stops the walk and is returned as is.
func ForEachSubtitleBlock(ctx context.Context, srcPath string, trackID uint64, fn func(SubtitleBlock) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	return walkSubtitleBlocks(ctx, srcPath, trackID, decodedBlocks(fn), opts)
}

// ForEachSubtitleBlockFrom is ForEachSubtitleBlock served from a prebuilt index: it seeks straight to each recorded
// block and reads nothing else of the file. The index must describe srcPath (size, Segment UID, timecode scale) and
// every block read must carry the track and the time the index recorded, or ErrIndexStale is returned; a track the
// index does not cover is ErrTrackNotIndexed. The blocks are served from the first: a codec whose state spans
// blocks needs them all.
func ForEachSubtitleBlockFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, fn func(SubtitleBlock) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	return indexedSubtitleBlocks(ctx, srcPath, trackID, ix, decodedBlocks(fn), opts)
}

// subtitleSink is what a consumer of a track's blocks gives the read.
type subtitleSink struct {
	// accept sees the container and the track before any block is read, and refuses a track it cannot serve.
	accept func(c *mkv.Container, t *mkv.Track) error
	// block takes a block as stored: its payload is not decompressed.
	block func(blk mkv.Block) error
	// lost is told a damaged region the read passed over starts at fromMs.
	lost func(fromMs int64) error
	// end, when set, is called once the last block is read: a consumer holding something back gives it then.
	end func() error
}

// finish calls the sink's end.
func (s subtitleSink) finish() error {
	if s.end == nil {
		return nil
	}
	return s.end()
}

// decodedBlocks is the sink of the public reads: any subtitle track, payloads decompressed.
func decodedBlocks(fn func(SubtitleBlock) error) subtitleSink {
	var track *mkv.Track
	return subtitleSink{
		accept: func(_ *mkv.Container, t *mkv.Track) error {
			track = t
			return nil
		},
		block: func(blk mkv.Block) error {
			data, err := decompressSubtitleBlock(track, blk.Data)
			if err != nil {
				return fmt.Errorf("subtitle block at %d ms: %w", blk.Timecode, err)
			}
			return fn(SubtitleBlock{TimeMs: blk.Timecode, DurationMs: blk.Duration, Data: data, Additions: blk.Additions})
		},
		lost: func(fromMs int64) error { return fn(SubtitleBlock{TimeMs: fromMs, Lost: true}) },
	}
}

// acceptSubtitleTrack finds the subtitle track trackID and shows it to the sink.
func acceptSubtitleTrack(c *mkv.Container, trackID uint64, sink subtitleSink) error {
	t := subtitleTrackByID(c, trackID)
	if t == nil || t.Type != mkv.SubtitleTrack {
		return fmt.Errorf("subtitle track %d not found", trackID)
	}
	return sink.accept(c, t)
}

// tellLost reports to the sink the regions the read passed over since the last call.
func tellLost(dmg *subtitleDamage, sink subtitleSink) error {
	fromMs, ok := dmg.skipped()
	if !ok {
		return nil
	}
	return sink.lost(fromMs)
}

// walkSubtitleBlocks reads the track's blocks by walking the whole file.
func walkSubtitleBlocks(ctx context.Context, srcPath string, trackID uint64, sink subtitleSink, opts []mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	if err := acceptSubtitleTrack(c, trackID, sink); err != nil {
		return err
	}
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return err
	}
	br.KeepTracks(trackID) // the reader skips the other tracks' payloads unread
	dmg := tolerateSubtitleDamage(br, opts)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		blk, err := br.Next()
		if err != nil && err != io.EOF {
			return err
		}
		if lerr := tellLost(dmg, sink); lerr != nil {
			return lerr
		}
		if err == io.EOF {
			return sink.finish()
		}
		if err := sink.block(blk); err != nil {
			return err
		}
	}
}

// indexedSubtitleBlocks reads the track's blocks where ix recorded them.
func indexedSubtitleBlocks(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, sink subtitleSink, opts []mkv.Options) error {
	if ix == nil {
		return fmt.Errorf("nil subtitle index")
	}
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenMetaWithFS(ctx, srcPath, fs)
	if err != nil {
		return err
	}
	st, err := fs.DoStat(srcPath)
	if err != nil {
		return err
	}
	if err := ix.checkAgainst(c, st.Size()); err != nil {
		return err
	}
	if err := acceptSubtitleTrack(c, trackID, sink); err != nil {
		return err
	}
	entries, ok := ix.entries[trackID]
	if !ok {
		return fmt.Errorf("%w: track %d", ErrTrackNotIndexed, trackID)
	}
	f, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var br *reader.BlockReader
	var dmg *subtitleDamage
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &entries[i]
		if br == nil {
			// The first position seats the reader and the rest re-seat it: one read window for the whole track.
			br, err = reader.NewBlockReaderFrom(f, ix.tcScale, e.pos)
			if err != nil {
				return err
			}
			br.KeepTracks(trackID)
			// A reader seated mid-file never passes over Tracks: without the strides a laced block's frames would
			// all carry the block's time, and the result would differ from the walk's.
			br.SetTrackDefaultDurations(reader.TrackDefaultDurations(c.Tracks))
			dmg = tolerateSubtitleDamage(br, opts)
		} else if err := br.SeekTo(e.pos); err != nil {
			return err
		}
		// One entry, e.frames frames: a laced block gives its whole lace without a new seek.
		for k := int64(0); k < e.frames; k++ {
			blk, err := br.Next()
			if dmg.pending() && (k > 0 || seatZeroed(f, e.pos.Off)) { // the entry lies in a damaged region: its blocks are lost
				if err := tellLost(dmg, sink); err != nil {
					return err
				}
				break
			}
			if err != nil {
				return fmt.Errorf("%w: reading the block at offset %d: %w", ErrIndexStale, e.pos.Off, err)
			}
			if blk.TrackNumber != trackID {
				return fmt.Errorf("%w: offset %d holds track %d, the index recorded track %d",
					ErrIndexStale, e.pos.Off, blk.TrackNumber, trackID)
			}
			// Only the first frame carries the recorded time; the rest are strided by the track's DefaultDuration.
			if k == 0 && blk.Timecode != e.timeMs {
				return fmt.Errorf("%w: offset %d holds track %d at %d ms, the index recorded %d ms",
					ErrIndexStale, e.pos.Off, trackID, blk.Timecode, e.timeMs)
			}
			if err := sink.block(blk); err != nil {
				return err
			}
		}
	}
	return sink.finish()
}
