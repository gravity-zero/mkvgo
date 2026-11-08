package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/gravity-zero/mkvgo/internal/oneopt"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

// Demux writes each requested track of the source to its own file in the output directory. The source is a
// Matroska/WebM, or an MP4: its tracks and blocks are then those RemuxFromMP4 would write to a Matroska, so the
// streams come out the same (ADTS, Annex-B, Ogg) from either container.
func Demux(ctx context.Context, opts mkv.DemuxOptions, extra ...mkv.Options) (err error) {
	if err := oneopt.Check(extra); err != nil {
		return err
	}
	isMP4, fs, err := opensLikeMP4(ctx, opts.SourcePath, mkv.FSFrom(extra))
	if err != nil {
		return err
	}
	if isMP4 {
		return demuxMP4(ctx, opts, fs, extra)
	}
	c, err := reader.OpenWithFS(ctx, opts.SourcePath, fs, reader.WithoutAttachmentData())
	if err != nil {
		return err
	}
	d := &demuxer{opts: opts, fs: fs, sync: mkv.SyncOutputFrom(extra)}
	if err := d.open(c); err != nil {
		return err
	}
	// A demux that fails leaves no truncated stream under its output names, nor
	// touches the files already there; every output is closed before any is
	// moved into place, so one that fails to close keeps them all out.
	defer d.settle(&err)

	f, err := fs.DoOpen(opts.SourcePath)
	if err != nil {
		return err
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return err
	}
	if p := mkv.ProgressFrom(extra); p != nil {
		if st, _ := fs.DoStat(opts.SourcePath); st != nil {
			br.SetProgress(p, st.Size())
		}
	}
	// Every format writes or copies a frame before the next one is read: one payload buffer serves them all.
	var payload []byte
	br.SetBlockBuffer(func(_ uint64, size int) []byte {
		if cap(payload) < size {
			payload = make([]byte, size)
		}
		return payload[:size]
	})
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
		if err := d.frame(blk); err != nil {
			return err
		}
	}
	return d.finish()
}

// demuxMP4 is Demux on an MP4: the tracks and blocks RemuxFromMP4 would write, through the same formats, under the
// remux's own rule for a track it cannot carry: refused by name, or dropped under DemuxOptions.SkipUnsupported and
// reported through OnDrop. TrackIDs naming other tracks leaves such a track out of the question; naming it fails
// the demux by name. The IDs, in TrackIDs as in the file names, are the MP4's own, the ones tracks lists.
func demuxMP4(ctx context.Context, opts mkv.DemuxOptions, fs *mkv.FS, extra []mkv.Options) (err error) {
	d := &demuxer{opts: opts, fs: fs, sync: mkv.SyncOutputFrom(extra), dropped: map[uint64]mp4.DroppedTrack{}}
	defer d.settle(&err)
	o := mp4.Options{FS: fs, Progress: mkv.ProgressFrom(extra), SkipUnsupported: opts.SkipUnsupported || len(opts.TrackIDs) > 0,
		OnDrop: func(t mp4.DroppedTrack) { d.dropped[t.ID] = t }}
	start := func(c *mkv.Container) error {
		for _, id := range opts.TrackIDs {
			if t, ok := d.dropped[id]; ok {
				return fmt.Errorf("track %d (%s) cannot be demuxed: %s", t.ID, t.Codec, t.Reason)
			}
		}
		if opts.OnDrop != nil && len(opts.TrackIDs) == 0 {
			for _, id := range sortedDropped(d.dropped) {
				t := d.dropped[id]
				opts.OnDrop(t.ID, t.Codec, t.Reason)
			}
		}
		return d.open(c)
	}
	if err := mp4.WalkBlocks(ctx, opts.SourcePath, start, d.frame, o); err != nil {
		return demuxOptionWords(err)
	}
	return d.finish()
}

// demuxOptionWords is a remux refusal as the demux's caller reads it: the option that drops the track is
// DemuxOptions.SkipUnsupported here. errors.Is and As still reach err.
func demuxOptionWords(err error) error {
	if !strings.Contains(err.Error(), "set Options.SkipUnsupported") {
		return err
	}
	return optionWords{err}
}

type optionWords struct{ error }

func (o optionWords) Error() string {
	return strings.ReplaceAll(o.error.Error(), "set Options.SkipUnsupported", "set DemuxOptions.SkipUnsupported")
}

func (o optionWords) Unwrap() error { return o.error }

// demuxID is the identifier a demux names t by, in TrackIDs as in the file name: the one the source file gives the
// track (a Matroska TrackNumber, an MP4 track_ID), which a track the MP4 remux drops does not shift.
func demuxID(t mkv.Track) uint64 {
	if t.FileTrackID != 0 {
		return t.FileTrackID
	}
	return t.ID
}

// sortedDropped is the dropped tracks' IDs in order.
func sortedDropped(m map[uint64]mp4.DroppedTrack) []uint64 {
	ids := make([]uint64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// demuxer holds a demux's outputs: one file per wanted track, in its format.
type demuxer struct {
	opts    mkv.DemuxOptions
	fs      *mkv.FS
	sync    bool
	wanted  map[uint64]mkv.Track
	formats map[uint64]*demuxFormat
	writers map[uint64]*bufio.Writer
	outputs []*output
	files   map[uint64]*output
	dropped map[uint64]mp4.DroppedTrack // MP4 tracks the remux cannot carry, left out
}

// open picks the tracks, creates their files and writes each format's head.
func (d *demuxer) open(c *mkv.Container) error {
	d.wanted = buildTrackSet(c, d.opts.TrackIDs)
	if len(d.wanted) == 0 {
		return fmt.Errorf("no matching tracks found")
	}
	if err := d.fs.DoMkdirAll(d.opts.OutputDir, 0755); err != nil {
		return err
	}
	d.formats = make(map[uint64]*demuxFormat, len(d.wanted))
	for id, t := range d.wanted {
		d.formats[id] = demuxFormatOf(t, d.opts.Raw)
	}
	var err error
	d.writers, d.outputs, err = openOutputFiles(d.wanted, d.formats, d.opts.OutputDir, d.opts.SourcePath, d.opts.NoOverwrite, d.fs, d.sync)
	if err != nil {
		return err
	}
	d.files = make(map[uint64]*output, len(d.outputs))
	for i, id := range sortedTracks(d.writers) { // the outputs were created in this order
		d.files[id] = d.outputs[i]
	}
	for _, id := range sortedTracks(d.writers) {
		if h := d.formats[id].head; h != nil {
			if err := h(d.writers[id]); err != nil {
				return fmt.Errorf("write track %d: %w", id, err)
			}
		}
	}
	return nil
}

// frame writes one block to its track's file, decoded and framed by the format.
func (d *demuxer) frame(blk mkv.Block) error {
	track, ok := d.wanted[blk.TrackNumber]
	if !ok {
		return nil
	}
	w := d.writers[blk.TrackNumber]
	data, err := track.DecodePayload(blk.Data)
	if err != nil {
		return fmt.Errorf("track %d: %w", demuxID(track), err)
	}
	if err := d.formats[blk.TrackNumber].frame(w, blk, data); err != nil {
		var fe *frameError
		if errors.As(err, &fe) {
			return fmt.Errorf("track %d, block at %.3f s: %w; demux with --raw (DemuxOptions.Raw) to keep the stored payloads",
				demuxID(track), float64(blk.Timecode)/1000, err)
		}
		return fmt.Errorf("write track %d: %w", demuxID(track), err)
	}
	return nil
}

// finish writes each format's end and flushes, so a final write error (a disk full) surfaces rather than being
// swallowed by the deferred close.
func (d *demuxer) finish() error {
	for _, id := range sortedTracks(d.writers) {
		if e := d.formats[id].end; e != nil {
			if err := e(d.writers[id]); err != nil {
				return fmt.Errorf("write track %d: %w", id, err)
			}
		}
	}
	for _, id := range sortedTracks(d.writers) {
		if err := d.writers[id].Flush(); err != nil {
			return fmt.Errorf("flush track %d: %w", id, err)
		}
		if fin := d.formats[id].finish; fin != nil {
			if err := fin(d.files[id]); err != nil {
				return fmt.Errorf("write track %d: %w", id, err)
			}
		}
	}
	return nil
}

// settle closes every output, then moves them into place; a failure keeps them all out.
func (d *demuxer) settle(err *error) {
	for _, o := range d.outputs {
		o.close(err)
	}
	for _, o := range d.outputs {
		o.commit(err)
	}
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
		idx[demuxID(t)] = t
	}
	for _, id := range trackIDs {
		if t, ok := idx[id]; ok {
			m[t.ID] = t // keyed by the block's TrackNumber
		}
	}
	return m
}

// demuxWriteBuf batches a track's frames into 1 MiB writes: a video frame
// alone is tens of KiB, and a write call costs the same whatever it carries.
const demuxWriteBuf = 1 << 20

// openOutputFiles creates one output per track, in track order. On a failure
// it closes and removes the files it had already created.
func openOutputFiles(tracks map[uint64]mkv.Track, formats map[uint64]*demuxFormat, dir, source string, noOverwrite bool, fs *mkv.FS, sync bool) (map[uint64]*bufio.Writer, []*output, error) {
	writers := make(map[uint64]*bufio.Writer, len(tracks))
	outputs := make([]*output, 0, len(tracks))
	if noOverwrite {
		for _, id := range sortedTracks(tracks) {
			path, err := safePath(dir, fmt.Sprintf("%d.%s", demuxID(tracks[id]), formats[id].ext))
			if err != nil {
				return nil, nil, err
			}
			if _, err := fs.DoStat(path); err == nil {
				return nil, nil, fmt.Errorf("%s already exists: remove it, or demux into another directory", path)
			}
		}
	}
	abandon := func(err error) (map[uint64]*bufio.Writer, []*output, error) {
		for _, o := range outputs {
			o.finish(&err)
		}
		return nil, nil, err
	}
	for _, id := range sortedTracks(tracks) {
		name := fmt.Sprintf("%d.%s", demuxID(tracks[id]), formats[id].ext)
		path, err := safePath(dir, name)
		if err != nil {
			return abandon(err)
		}
		f, err := createOutput(fs, sync, path, source)
		if err != nil {
			return abandon(err)
		}
		writers[id] = bufio.NewWriterSize(f, demuxWriteBuf)
		outputs = append(outputs, f)
	}
	return writers, outputs, nil
}

// demuxCodecName is the codec an output file is named after: mkvgo's short
// name when the track has one, otherwise the codec its CodecID resolves to
// (theora for V_THEORA), and the CodecID itself only when nothing resolves it.
func demuxCodecName(t mkv.Track) string {
	for _, prefix := range []string{"V_", "A_", "S_"} {
		if strings.HasPrefix(t.Codec, prefix) {
			return t.FFprobeCodecName()
		}
	}
	return t.Codec
}
