package ops

// subpgs.go - extraction of HDMV PGS (S_HDMV/PGS) bitmap subtitle tracks. It is
// the bitmap sibling of subtitle.go's WebVTT extraction: the same read paths,
// walking or served from a SubtitleIndex, but the payload decodes to pictures
// rather than to text. The index needs nothing new - it filters on track TYPE,
// never on codec, so a PGS track built into an index by an earlier release is
// served here without a rebuild.

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// PGSCue is one bitmap subtitle: when it is shown, where on the subtitle plane,
// and the picture itself with its palette already applied.
//
// Image is *image.NRGBA, not *image.RGBA: a PGS palette carries STRAIGHT alpha,
// while Go's image.RGBA is alpha-premultiplied by convention, so straight values
// stored there would draw and encode wrong. It satisfies image.Image, and
// png.Encode writes it without a conversion.
type PGSCue struct {
	StartMs, EndMs int64
	// X, Y is the top-left corner of Image on the subtitle plane, as the
	// composition gives it.
	X, Y int
	// ScreenW, ScreenH is the plane X, Y and the picture are expressed in. It is
	// the disc's subtitle resolution, which is NOT necessarily the video track's
	// - a caller that scales the picture to its player must scale by this, not
	// by the video's width and height.
	ScreenW, ScreenH int
	// Forced marks a cue the disc wants shown even with subtitles off (signs,
	// foreign dialogue). It is per cue: a single track can carry both kinds, so
	// the track's own forced flag cannot answer this.
	Forced bool
	Image  *image.NRGBA
}

// ExtractSubtitlePGS decodes the PGS subtitle track trackID of the Matroska file
// at srcPath, walking the file once.
//
// MEMORY: the result holds every cue's picture at once. A feature film's PGS
// track is on the order of 1500 cues of roughly 1920x150 pixels, which is about
// 1.7 GB of decoded NRGBA - more than mkvgo's whole budget. Use
// ForEachSubtitlePGS unless the track is known to be short.
func ExtractSubtitlePGS(ctx context.Context, srcPath string, trackID uint64, opts ...mkv.Options) ([]PGSCue, error) {
	var cues []PGSCue
	err := ForEachSubtitlePGS(ctx, srcPath, trackID, func(c PGSCue) error {
		cues = append(cues, c)
		return nil
	}, opts...)
	if err != nil {
		return nil, err
	}
	return cues, nil
}

// ForEachSubtitlePGS walks srcPath once and calls fn with each decoded cue, in
// presentation order. Only one cue's picture is alive at a time, so a whole
// track can be written out in bounded memory. An error from fn stops the walk
// and is returned as is.
func ForEachSubtitlePGS(ctx context.Context, srcPath string, trackID uint64, fn func(PGSCue) error, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	c, err := reader.OpenWithFS(ctx, srcPath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	if err := checkPGSTrack(c, trackID); err != nil {
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
	br.KeepTracks(trackID) // reader-side filter, as in ExtractSubtitleWebVTT

	asm := newPGSAssembler(fn)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := asm.block(blk.Timecode, blk.Duration, blk.Data); err != nil {
			return err
		}
	}
	return asm.finish()
}

// ExtractSubtitlePGSFrom is ExtractSubtitlePGS served from a prebuilt index:
// instead of walking the file it seeks straight to each recorded block. The same
// staleness checks as ExtractSubtitleWebVTTFrom apply, and for the same reason -
// a stale position would decode whatever bytes happen to sit there.
//
// MEMORY: as ExtractSubtitlePGS - see ForEachSubtitlePGSFrom for the bounded
// form.
func ExtractSubtitlePGSFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, opts ...mkv.Options) ([]PGSCue, error) {
	var cues []PGSCue
	err := ForEachSubtitlePGSFrom(ctx, srcPath, trackID, ix, func(c PGSCue) error {
		cues = append(cues, c)
		return nil
	}, opts...)
	if err != nil {
		return nil, err
	}
	return cues, nil
}

// ForEachSubtitlePGSFrom serves one PGS track from a prebuilt SubtitleIndex,
// calling fn with each decoded cue in presentation order.
//
// The index is read from its first entry onwards, never from the middle: PGS
// state (palettes, objects) spans display sets within an epoch, so a cue decoded
// from a mid-track start could reference pictures that were never read.
func ForEachSubtitlePGSFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, fn func(PGSCue) error, opts ...mkv.Options) error {
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
	if err := checkPGSTrack(c, trackID); err != nil {
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

	asm := newPGSAssembler(fn)
	var br *reader.BlockReader
	for i := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := &entries[i]
		if br == nil {
			// The first position seats the reader; the rest re-seat it, so the
			// read window is allocated once for the whole track.
			br, err = reader.NewBlockReaderFrom(f, ix.tcScale, e.pos)
			if err != nil {
				return err
			}
			br.KeepTracks(trackID)
			// A mid-file reader never walks over the Tracks element, so it
			// cannot pick the per-frame strides up on its own.
			br.SetTrackDefaultDurations(reader.TrackDefaultDurations(c.Tracks))
		} else if err := br.SeekTo(e.pos); err != nil {
			return err
		}
		for k := int64(0); k < e.frames; k++ {
			blk, err := br.Next()
			if err != nil {
				return fmt.Errorf("%w: reading the block at offset %d: %w", ErrIndexStale, e.pos.Off, err)
			}
			if blk.TrackNumber != trackID {
				return fmt.Errorf("%w: offset %d holds track %d, the index recorded track %d",
					ErrIndexStale, e.pos.Off, blk.TrackNumber, trackID)
			}
			// Only the block's first frame carries the recorded timecode; the
			// rest are strided by the track's DefaultDuration, which a mid-file
			// reader cannot know - so they are not checked against the index.
			if k == 0 && blk.Timecode != e.timeMs {
				return fmt.Errorf("%w: offset %d holds track %d at %d ms, the index recorded %d ms",
					ErrIndexStale, e.pos.Off, trackID, blk.Timecode, e.timeMs)
			}
			if err := asm.block(blk.Timecode, blk.Duration, blk.Data); err != nil {
				return err
			}
		}
	}
	return asm.finish()
}

// checkPGSTrack resolves a track that must exist, be a subtitle track, and carry
// PGS. The message names the codec it did find, because the caller's next move
// differs by codec: a text track goes to ExtractSubtitleWebVTT instead.
func checkPGSTrack(c *mkv.Container, trackID uint64) error {
	for _, t := range c.Tracks {
		if t.ID != trackID || t.Type != mkv.SubtitleTrack {
			continue
		}
		if t.Codec != "pgs" {
			if isTextSubtitle(t.Codec) {
				return fmt.Errorf("subtitle track %d codec %q is text, not PGS bitmaps - extract it as WebVTT instead", trackID, t.Codec)
			}
			return fmt.Errorf("subtitle track %d codec %q is not PGS (S_HDMV/PGS)", trackID, t.Codec)
		}
		return nil
	}
	return fmt.Errorf("subtitle track %d not found", trackID)
}

// pgsAssembler turns the stream of decoded display sets into timed cues. A PGS
// cue's END time is not in its own block: it is the timecode of the next display
// set, which is normally an empty composition meaning "clear the screen". So a
// cue is held open until the block that ends it arrives, and emitted then - one
// cue of latency, never the whole track.
type pgsAssembler struct {
	dec  *subtitle.PGSDecoder
	emit func(PGSCue) error
	open *PGSCue
}

func newPGSAssembler(fn func(PGSCue) error) *pgsAssembler {
	return &pgsAssembler{dec: subtitle.NewPGSDecoder(), emit: fn}
}

func (a *pgsAssembler) block(timeMs, durMs int64, data []byte) error {
	sets, err := a.dec.Decode(data)
	if err != nil {
		return fmt.Errorf("subtitle block at %d ms: %w", timeMs, err)
	}
	for i := range sets {
		ds := &sets[i]
		// Whatever is on screen ends where this display set begins, whether the
		// new one clears the screen or replaces it.
		if err := a.closeAt(timeMs); err != nil {
			return err
		}
		if ds.Clear() {
			continue
		}
		cue := flattenPGSDisplaySet(ds, timeMs)
		if durMs > 0 {
			// A muxer that wrote a BlockDuration knows the end better than the
			// next block's timecode does: emit immediately and hold nothing.
			cue.EndMs = timeMs + durMs
			if err := a.emit(cue); err != nil {
				return err
			}
			continue
		}
		a.open = &cue
	}
	return nil
}

// closeAt ends the open cue at t. A t that does not move the clock forward (two
// display sets inside one block) would make a zero-length cue, so the default
// duration is used instead - the same fallback the text path applies.
func (a *pgsAssembler) closeAt(t int64) error {
	if a.open == nil {
		return nil
	}
	cue := *a.open
	a.open = nil
	cue.EndMs = t
	if cue.EndMs <= cue.StartMs {
		cue.EndMs = cue.StartMs + defaultSubDurationMs
	}
	return a.emit(cue)
}

// finish flushes a cue the track never closed - a last subtitle with no trailing
// "clear" display set, which is common enough not to be an error.
func (a *pgsAssembler) finish() error {
	if a.open == nil {
		return nil
	}
	cue := *a.open
	a.open = nil
	cue.EndMs = cue.StartMs + defaultSubDurationMs
	return a.emit(cue)
}

// flattenPGSDisplaySet reduces a display set to the one picture a PGSCue holds.
// Most sets carry a single object and are handed over untouched; a set with
// several (a disc that puts a top and a bottom line in two windows) is composited
// onto the smallest rectangle covering them, so X, Y stays a real screen position
// rather than becoming meaningless.
func flattenPGSDisplaySet(ds *subtitle.PGSDisplaySet, startMs int64) PGSCue {
	cue := PGSCue{StartMs: startMs, ScreenW: ds.ScreenW, ScreenH: ds.ScreenH}
	if len(ds.Objects) == 1 {
		o := ds.Objects[0]
		cue.X, cue.Y, cue.Forced, cue.Image = o.X, o.Y, o.Forced, o.Image
		return cue
	}
	var union image.Rectangle
	for _, o := range ds.Objects {
		r := o.Image.Bounds().Add(image.Pt(o.X, o.Y))
		union = union.Union(r)
		cue.Forced = cue.Forced || o.Forced
	}
	out := image.NewNRGBA(image.Rect(0, 0, union.Dx(), union.Dy()))
	for _, o := range ds.Objects {
		at := image.Pt(o.X-union.Min.X, o.Y-union.Min.Y)
		draw.Draw(out, image.Rectangle{Min: at, Max: at.Add(o.Image.Bounds().Size())}, o.Image, image.Point{}, draw.Over)
	}
	cue.X, cue.Y, cue.Image = union.Min.X, union.Min.Y, out
	return cue
}
