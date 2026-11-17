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

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
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
	if err := oneopt.Check(opts); err != nil {
		return nil, err
	}
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

// ForEachSubtitlePGS is ForEachSubtitleBitmap for a track that must be PGS: any other codec is refused by name.
func ForEachSubtitlePGS(ctx context.Context, srcPath string, trackID uint64, fn func(PGSCue) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	return walkSubtitleBlocks(ctx, srcPath, trackID, bitmapCues(fn, checkPGSTrack), opts)
}

// ExtractSubtitlePGSFrom is ExtractSubtitlePGS served from a prebuilt index:
// instead of walking the file it seeks straight to each recorded block.
//
// MEMORY: as ExtractSubtitlePGS - see ForEachSubtitlePGSFrom for the bounded
// form.
func ExtractSubtitlePGSFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, opts ...mkv.Options) ([]PGSCue, error) {
	if err := oneopt.Check(opts); err != nil {
		return nil, err
	}
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

// ForEachSubtitlePGSFrom is ForEachSubtitleBitmapFrom for a track that must be PGS.
func ForEachSubtitlePGSFrom(ctx context.Context, srcPath string, trackID uint64, ix *SubtitleIndex, fn func(PGSCue) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	return indexedSubtitleBlocks(ctx, srcPath, trackID, ix, bitmapCues(fn, checkPGSTrack), opts)
}

// checkPGSTrack refuses a subtitle track that is not PGS. The message names the codec it did find, because the
// caller's next move differs by codec: a text track goes to ExtractSubtitleWebVTT instead.
func checkPGSTrack(t *mkv.Track) error {
	if t.Codec == "pgs" {
		return nil
	}
	if isTextSubtitle(t.Codec) {
		return fmt.Errorf("subtitle track %d codec %q is text, not PGS bitmaps - extract it as WebVTT instead", t.ID, t.Codec)
	}
	return fmt.Errorf("subtitle track %d codec %q is not PGS (S_HDMV/PGS)", t.ID, t.Codec)
}
