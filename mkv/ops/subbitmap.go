package ops

// subbitmap.go - bitmap subtitle tracks as timed pictures, whatever the codec: PGS (Blu-ray), VobSub (DVD) and DVB
// (broadcast) each decode to display sets (mkv/subtitle), and the rules that turn display sets into cues are the
// same for the three. The blocks come from the read of subblocks.go, walked or served from a SubtitleIndex.

import (
	"context"
	"fmt"
	"image"
	"image/draw"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// BitmapCue is one bitmap subtitle of any codec: the type PGSCue names.
type BitmapCue = PGSCue

// ForEachSubtitleBitmap walks srcPath once and calls fn with each cue of the bitmap subtitle track trackID, in
// presentation order: a PGS, VobSub or DVB track, decoded by its codec. Only one cue's picture is alive at a time,
// so a whole track is written out in bounded memory. An error from fn stops the walk and is returned as is.
//
// A cue starts where its display set takes effect and ends, in this order: where the stream says (a VobSub stop
// command), at the duration the muxer wrote for its block, or where the next display set begins - no later than a
// DVB page's time-out, and 3 s after its start for a last cue nothing closes. A display set of several pictures is
// one cue, composited on the smallest rectangle that holds them.
//
// A block that reads but does not decode is lost alone: the cue on screen ends at its time, the decoder starts
// afresh and the block is reported through Options.OnSkip, the read going on - unless Options.DamageBytes refuses
// any damage, which fails the read on it. A DVB decoder that starts afresh holds no region and no colour table, so
// nothing shows until the stream's next acquisition point or mode change.
func ForEachSubtitleBitmap(ctx context.Context, srcPath string, trackID uint64, fn func(BitmapCue) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	return walkSubtitleBlocks(ctx, srcPath, trackID, bitmapCues(fn, nil, opts), opts)
}

// ForEachSubtitleBitmapFrom is ForEachSubtitleBitmap served from a prebuilt index, under the checks of
// ForEachSubtitleBlockFrom. The index is read from its first entry: the state of a bitmap codec spans blocks, and a
// cue decoded from the middle of a track could draw on pictures that were never read.
func ForEachSubtitleBitmapFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, fn func(BitmapCue) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	return indexedSubtitleBlocks(ctx, srcPath, trackID, ix, bitmapCues(fn, nil, opts), opts)
}

// bitmapDecoder is what the three codecs' decoders share.
type bitmapDecoder interface {
	Decode(block []byte) ([]subtitle.BitmapDisplaySet, error)
	Reset()
}

// newBitmapDecoder picks the decoder of a subtitle track's codec, and the name its errors go by.
func newBitmapDecoder(t *mkv.Track) (bitmapDecoder, string, error) {
	switch t.Codec {
	case "pgs":
		return subtitle.NewPGSDecoder(), "PGS", nil
	case "vobsub":
		d, err := subtitle.NewVobSubDecoder(t.CodecPrivate)
		return d, "VobSub", err
	case "dvbsub":
		return subtitle.NewDVBSubDecoder(t.CodecPrivate), "DVB subtitle", nil
	}
	if isTextSubtitle(t.Codec) {
		return nil, "", fmt.Errorf("subtitle track %d codec %q is text, not bitmaps - extract it as WebVTT instead", t.ID, t.Codec)
	}
	return nil, "", fmt.Errorf("subtitle track %d codec %q is not a bitmap subtitle this reads (PGS, VobSub, DVB)", t.ID, t.Codec)
}

// bitmapCues is the sink that decodes a bitmap track's blocks into cues; check, when set, refuses the track first.
func bitmapCues(fn func(BitmapCue) error, check func(*mkv.Track) error, opts []mkv.Options) subtitleSink {
	var asm *bitmapAssembler
	return subtitleSink{
		accept: func(_ *mkv.Container, t *mkv.Track) error {
			if check != nil {
				if err := check(t); err != nil {
					return err
				}
			}
			a, err := newBitmapAssembler(fn, t, opts)
			asm = a
			return err
		},
		block: func(blk mkv.Block) error { return asm.block(blk) },
		lost:  func(fromMs int64) error { return asm.lost(fromMs) },
		end:   func() error { return asm.finish() },
	}
}

// bitmapAssembler turns the stream of decoded display sets into timed cues. A cue's end is often not in its own
// block: it is the time of the next display set, which is normally an empty one meaning "clear the screen". So a
// cue is held open until the block that ends it arrives, and emitted then - one cue of latency, never the whole
// track.
type bitmapAssembler struct {
	dec    bitmapDecoder
	name   string
	emit   func(BitmapCue) error
	track  *mkv.Track
	open   *BitmapCue
	latest int64 // when the open cue ends at the latest: its page's time-out, 0 for none
	unitMs int64 // the time of the block that started the unit being decoded
	strict bool  // a block that does not decode fails the read: Options.DamageBytes refuses any damage
	onSkip func(mkv.DamagedRange)
}

func newBitmapAssembler(fn func(BitmapCue) error, t *mkv.Track, opts []mkv.Options) (*bitmapAssembler, error) {
	dec, name, err := newBitmapDecoder(t)
	if err != nil {
		return nil, err
	}
	return &bitmapAssembler{dec: dec, name: name, emit: fn, track: t,
		strict: mkv.DamageBytesFrom(opts) < 0, onSkip: mkv.OnSkipFrom(opts)}, nil
}

// block decodes one block and emits the cues it completes. A block that does not decode is lost alone: decoding
// it is one thing, and what the caller's function returns for a cue is another, which always stops the read.
func (a *bitmapAssembler) block(blk mkv.Block) error {
	shown, err := a.decode(blk)
	if err != nil {
		return a.undecodable(blk, err)
	}
	for i := range shown {
		s := &shown[i]
		// Whatever is on screen ends where this display set begins, whether the new one clears the screen or
		// replaces it.
		if err := a.closeAt(s.cue.StartMs); err != nil {
			return err
		}
		if s.clear {
			continue
		}
		if s.cue.EndMs > s.cue.StartMs { // the stream or the muxer gave the end: emit and hold nothing
			if err := a.emit(s.cue); err != nil {
				return err
			}
			continue
		}
		cue := s.cue
		a.open, a.latest = &cue, s.latest
	}
	return nil
}

// decodedSet is one display set of a block, decoded and ready to be timed against what is on screen.
type decodedSet struct {
	cue    BitmapCue // StartMs set; EndMs when the stream or the muxer gave it, the picture unless clear
	clear  bool
	latest int64 // when the cue ends at the latest, 0 for none
}

// decode turns a block into its display sets, flattened; nothing is emitted, so an error loses the block whole.
func (a *bitmapAssembler) decode(blk mkv.Block) ([]decodedSet, error) {
	timeMs := blk.Timecode
	data, err := decompressSubtitleBlock(a.track, blk.Data)
	if err != nil {
		return nil, fmt.Errorf("subtitle block at %d ms: %w", timeMs, err)
	}
	// A unit spread over several blocks (VobSub) is dated by the first of them.
	if c, ok := a.dec.(interface{ Continues([]byte) bool }); !ok || !c.Continues(data) {
		a.unitMs = timeMs
	}
	sets, err := a.dec.Decode(data)
	if err != nil { // the sets a decoder hands back with its error go with the block
		return nil, fmt.Errorf("subtitle block at %d ms: %w", timeMs, err)
	}
	shown := make([]decodedSet, 0, len(sets))
	for i := range sets {
		ds := &sets[i]
		start := a.unitMs + ds.Delay.Milliseconds()
		if ds.Clear() {
			shown = append(shown, decodedSet{cue: BitmapCue{StartMs: start}, clear: true})
			continue
		}
		cue, err := flattenDisplaySet(ds, start, a.name)
		if err != nil {
			return nil, fmt.Errorf("subtitle block at %d ms: %w", timeMs, err)
		}
		// The stream's own end, then the muxer's.
		switch {
		case ds.Duration > 0:
			cue.EndMs = start + ds.Duration.Milliseconds()
		case blk.Duration > 0:
			cue.EndMs = timeMs + blk.Duration
		}
		s := decodedSet{cue: cue}
		if ds.Timeout > 0 {
			s.latest = start + ds.Timeout.Milliseconds()
		}
		shown = append(shown, s)
	}
	return shown, nil
}

// undecodable loses a block that reads but does not decode: the decoder forgets its state, the cue on screen ends
// at the block's time and the block is reported as a damaged region. A read that refuses any damage fails on it.
func (a *bitmapAssembler) undecodable(blk mkv.Block, err error) error {
	if a.strict {
		return err
	}
	a.dec.Reset()
	if err := a.closeAt(blk.Timecode); err != nil {
		return err
	}
	if a.onSkip != nil {
		a.onSkip(mkv.DamagedRange{StartOffset: blk.DataOffset, EndOffset: blk.DataOffset + int64(len(blk.Data)),
			ApproxStartMs: blk.Timecode, ApproxEndMs: blk.Timecode})
	}
	return nil
}

// closeAt ends the open cue at t, or at its time-out when that comes first. A t that does not move the clock
// forward (two display sets inside one block) would make a zero-length cue, so the default duration is used
// instead - the same fallback the text path applies.
func (a *bitmapAssembler) closeAt(t int64) error {
	if a.open == nil {
		return nil
	}
	cue := *a.open
	a.open = nil
	cue.EndMs = t
	if a.latest > 0 && cue.EndMs > a.latest {
		cue.EndMs = a.latest
	}
	if cue.EndMs <= cue.StartMs {
		cue.EndMs = cue.StartMs + defaultSubDurationMs
	}
	return a.emit(cue)
}

// lost ends the cue on screen where a region the read just passed over begins, and forgets the state it cut.
func (a *bitmapAssembler) lost(fromMs int64) error {
	a.dec.Reset()
	return a.closeAt(fromMs)
}

// finish flushes a cue the track never closed - a last subtitle with no trailing "clear" display set, which is
// common enough not to be an error.
func (a *bitmapAssembler) finish() error {
	if a.open == nil {
		return nil
	}
	cue := *a.open
	a.open = nil
	cue.EndMs = cue.StartMs + defaultSubDurationMs
	if a.latest > 0 && a.latest < cue.EndMs {
		cue.EndMs = a.latest
	}
	return a.emit(cue)
}

// flattenDisplaySet reduces a display set to the one picture a cue holds. Most sets carry a single object and are
// handed over untouched; a set with several (a disc that puts a top and a bottom line in two windows, a broadcast
// with one region a line) is composited onto the smallest rectangle covering them, so X, Y stays a real screen
// position rather than becoming meaningless.
func flattenDisplaySet(ds *subtitle.BitmapDisplaySet, startMs int64, name string) (BitmapCue, error) {
	cue := BitmapCue{StartMs: startMs, ScreenW: ds.ScreenW, ScreenH: ds.ScreenH}
	if len(ds.Objects) == 1 {
		o := ds.Objects[0]
		cue.X, cue.Y, cue.Forced, cue.Image = o.X, o.Y, o.Forced, o.Image
		return cue, nil
	}
	var union image.Rectangle
	for _, o := range ds.Objects {
		r := o.Image.Bounds().Add(image.Pt(o.X, o.Y))
		union = union.Union(r)
		cue.Forced = cue.Forced || o.Forced
	}
	if union.Dx() > bitmapMaxPlane || union.Dy() > bitmapMaxPlane {
		return cue, fmt.Errorf("%s: a display set spreads its objects over %dx%d pixels, past the %d a subtitle plane holds: the track is damaged or forged", name, union.Dx(), union.Dy(), bitmapMaxPlane)
	}
	out := image.NewNRGBA(image.Rect(0, 0, union.Dx(), union.Dy()))
	for _, o := range ds.Objects {
		at := image.Pt(o.X-union.Min.X, o.Y-union.Min.Y)
		draw.Draw(out, image.Rectangle{Min: at, Max: at.Add(o.Image.Bounds().Size())}, o.Image, image.Point{}, draw.Over)
	}
	cue.X, cue.Y, cue.Image = union.Min.X, union.Min.Y, out
	return cue, nil
}

// bitmapMaxPlane bounds the picture several objects are composited onto, as the formats bound each object: placed
// far apart by a forged composition, two dots would otherwise ask for a picture of gigabytes.
const bitmapMaxPlane = 4096
