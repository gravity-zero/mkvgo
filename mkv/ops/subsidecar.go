package ops

// subsidecar.go - the bitmap subtitles that sit beside a video, a VobSub .idx/.sub pair or a PGS .sup, as the same
// timed pictures ForEachSubtitleBitmap gives for a track of the file: the readers of mkv/subtitle hand the units and
// display sets over, the assembler of subbitmap.go dates and ends the cues, under the same rules.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// ForEachSubtitleBitmapFile calls fn with each cue of a bitmap subtitle file that sits beside a video, in
// presentation order: a PGS .sup, or a VobSub pair named by its .idx or its .sub, the other found beside it under
// the same name. stream picks the language of a .idx, by its position in it from 0 (VobSubIndex.Streams); a .sup
// holds one, 0. The cues are timed and ended as ForEachSubtitleBitmap times and ends a track's, a unit or a display
// set that does not decode is lost alone and reported through Options.OnSkip (without a byte range: StartOffset and
// EndOffset are -1), and a negative Options.DamageBytes fails the read on it instead. A file the reader cannot go
// through to its end (cut short, a unit the .sub holds in part) gives the cues ahead of the fault, then the error.
// One cue's picture is alive at a time; an error from fn stops the read and is returned as is.
func ForEachSubtitleBitmapFile(ctx context.Context, path string, stream int, fn func(BitmapCue) error, opts ...mkv.Options) error {
	if err := oneopt.Check(opts); err != nil {
		return err
	}
	fs := mkv.FSFrom(opts)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sup":
		if stream != 0 {
			return fmt.Errorf("%s holds one subtitle stream, 0, not %d", path, stream)
		}
		return supCues(ctx, fs, path, fn, opts)
	case ".idx", ".sub":
		return vobsubPairCues(ctx, fs, path, stream, fn, opts)
	}
	return fmt.Errorf("%s is not a bitmap subtitle file this reads: a PGS .sup, or a VobSub .idx with its .sub", path)
}

// supCues reads a .sup display set by display set.
func supCues(ctx context.Context, fs *mkv.FS, path string, fn func(BitmapCue) error, opts []mkv.Options) error {
	f, err := fs.DoOpen(path)
	if err != nil {
		return err
	}
	defer f.Close()
	emit, stopped := callerErrors(fn)
	asm, err := newBitmapAssembler(emit, &mkv.Track{Codec: "pgs"}, opts)
	if err != nil {
		return err
	}
	readErr := subtitle.ForEachSUPDisplaySet(f, func(timeMs int64, set []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return asm.block(mkv.Block{Timecode: timeMs, Data: set, DataOffset: -1})
	})
	return finishSidecar(ctx, asm, readErr, *stopped)
}

// vobsubPairCues reads one language of a .idx/.sub pair unit by unit.
func vobsubPairCues(ctx context.Context, fs *mkv.FS, path string, stream int, fn func(BitmapCue) error, opts []mkv.Options) error {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	idxPath, subPath := base+".idx", base+".sub"
	if ext == strings.ToUpper(ext) { // a pair named in capitals, as DVD tools write it
		idxPath, subPath = base+".IDX", base+".SUB"
	}
	idx, err := fs.DoOpen(idxPath)
	if err != nil {
		return fmt.Errorf("a VobSub .sub is read with the .idx beside it: %w", err)
	}
	ix, err := subtitle.ParseVobSubIndex(idx)
	idx.Close()
	if err != nil {
		return err
	}
	if stream < 0 || stream >= len(ix.Streams) {
		return fmt.Errorf("%s holds %d subtitle stream(s), from 0: no stream %d", idxPath, len(ix.Streams), stream)
	}
	sub, err := fs.DoOpen(subPath)
	if err != nil {
		return fmt.Errorf("a VobSub .idx is read with the .sub beside it: %w", err)
	}
	defer sub.Close()
	emit, stopped := callerErrors(fn)
	asm, err := newBitmapAssembler(emit, &mkv.Track{Codec: "vobsub", CodecPrivate: ix.Header}, opts)
	if err != nil {
		return err
	}
	readErr := ix.ForEachUnit(seekReaderAt{sub}, stream, func(timeMs int64, unit []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return asm.block(mkv.Block{Timecode: timeMs, Data: unit, DataOffset: -1})
	})
	return finishSidecar(ctx, asm, readErr, *stopped)
}

// callerErrors wraps fn so that what it returns is kept: the read tells a fault of the file from the caller's stop.
func callerErrors(fn func(BitmapCue) error) (func(BitmapCue) error, *error) {
	stopped := new(error)
	return func(c BitmapCue) error {
		err := fn(c)
		if err != nil {
			*stopped = err
		}
		return err
	}, stopped
}

// finishSidecar gives the cue still on screen once the file is read, or once the reader stopped on a fault of the
// file, whose error then follows; the caller's own error and a cancelled read stop at once.
func finishSidecar(ctx context.Context, asm *bitmapAssembler, readErr, stopped error) error {
	if stopped != nil || ctx.Err() != nil {
		return readErr
	}
	if err := asm.finish(); err != nil {
		return err
	}
	return readErr
}

// seekReaderAt reads at an offset through a file that only seeks, io.EOF for fewer bytes than asked, as os.File.ReadAt.
type seekReaderAt struct{ f io.ReadSeeker }

func (r seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := r.f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.f, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}
