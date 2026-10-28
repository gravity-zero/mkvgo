package ops

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

func Demux(ctx context.Context, opts mkv.DemuxOptions, extra ...mkv.Options) (err error) {
	fs := mkv.FSFrom(extra)
	c, err := reader.OpenWithFS(ctx, opts.SourcePath, fs, reader.WithoutAttachmentData())
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

	writers, outputs, err := openOutputFiles(wanted, opts.OutputDir, opts.SourcePath, opts.NoOverwrite, fs, mkv.SyncOutputFrom(extra))
	if err != nil {
		return err
	}
	// A demux that fails leaves no truncated stream under its output names, nor
	// touches the files already there; every output is closed before any is
	// moved into place, so one that fails to close keeps them all out.
	defer func() {
		for _, o := range outputs {
			o.close(&err)
		}
		for _, o := range outputs {
			o.commit(&err)
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
	if p := mkv.ProgressFrom(extra); p != nil {
		if st, _ := fs.DoStat(opts.SourcePath); st != nil {
			br.SetProgress(p, st.Size())
		}
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
		data, err := track.DecodePayload(blk.Data)
		if err != nil {
			return fmt.Errorf("track %d: %w", blk.TrackNumber, err)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write track %d: %w", blk.TrackNumber, err)
		}
	}
	// Flush buffered writers so a final write error (e.g. disk full) is surfaced
	// rather than swallowed by the deferred Close.
	for _, id := range sortedTracks(writers) {
		if err := writers[id].Flush(); err != nil {
			return fmt.Errorf("flush track %d: %w", id, err)
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

// demuxWriteBuf batches a track's frames into 1 MiB writes: a video frame
// alone is tens of KiB, and a write call costs the same whatever it carries.
const demuxWriteBuf = 1 << 20

// openOutputFiles creates one output per track, in track order. On a failure
// it closes and removes the files it had already created.
func openOutputFiles(tracks map[uint64]mkv.Track, dir, source string, noOverwrite bool, fs *mkv.FS, sync bool) (map[uint64]*bufio.Writer, []*output, error) {
	writers := make(map[uint64]*bufio.Writer, len(tracks))
	outputs := make([]*output, 0, len(tracks))
	if noOverwrite {
		for _, id := range sortedTracks(tracks) {
			path, err := safePath(dir, fmt.Sprintf("%d.%s", id, sanitizeCodec(demuxCodecName(tracks[id]))))
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
		name := fmt.Sprintf("%d.%s", id, sanitizeCodec(demuxCodecName(tracks[id])))
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
