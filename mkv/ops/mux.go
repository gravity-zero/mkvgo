package ops

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func Mux(ctx context.Context, opts mkv.MuxOptions, extra ...mkv.Options) (err error) {
	fs := mkv.FSFrom(extra)
	out, err := fs.DoCreate(opts.OutputPath)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	// Surface a Close error on the success path (e.g. a custom FS that finalises
	// the write on Close) instead of silently dropping it.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	tracks, trackMap, err := buildMuxTracks(ctx, opts.Tracks, fs)
	if err != nil {
		return err
	}

	sources, timecodeScale, durationMs, err := buildMuxSources(ctx, opts.Tracks, trackMap, fs)
	if err != nil {
		return err
	}

	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: timecodeScale,
			Title:         opts.Title,
			MuxingApp:     "mkvgo",
			WritingApp:    "mkvgo",
		},
		Tracks:      tracks,
		Chapters:    opts.Chapters,
		Attachments: opts.Attachments,
		DurationMs:  durationMs,
	}

	mw := writer.NewMKVWriter(out)
	if err := mw.WriteStart(); err != nil {
		return err
	}
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		return err
	}
	stats, err := streamMergeToWriter(ctx, mw, timecodeScale, fs, sources, mkv.ProgressFrom(extra))
	if err != nil {
		return err
	}
	// One Tags element after the clusters: the caller's tags plus the
	// convention per-track statistics accumulated during the stream. The
	// SeekHead points to it, so head-only readers (WithBitrate) still find it.
	tags := append(append([]mkv.Tag{}, opts.Tags...), statsTags(tracks, stats)...)
	if err := mw.WriteTagsElement(tags); err != nil {
		return err
	}
	return mw.Finalize()
}

// statsTags builds convention per-track statistics tags (BPS, DURATION,
// NUMBER_OF_FRAMES, NUMBER_OF_BYTES), keyed by track UID - the values probers
// surfaces as TAG:BPS etc. and matroska.WithBitrate reads back head-only.
func statsTags(tracks []mkv.Track, stats map[uint64]*trackStats) []mkv.Tag {
	var out []mkv.Tag
	for _, t := range tracks {
		// A declared track that received nothing reports zero, it does not
		// vanish: that is the truth about the output, and it matches the
		// empty-stream content hash the same track gets.
		s := stats[t.ID]
		if s == nil {
			s = &trackStats{}
		}
		uid := t.UID
		if uid == 0 {
			uid = t.ID // the writer defaults a zero UID to the track ID
		}
		// A frame count and a byte count are true whatever the track's duration
		// is; only the rate and the duration itself need one. Declining the whole
		// set when a part holds a single frame of a track - so its measured
		// duration is 0 - deleted the source's statistics and put nothing back.
		names := []string{"NUMBER_OF_FRAMES", "NUMBER_OF_BYTES"}
		simple := []mkv.SimpleTag{
			{Name: "NUMBER_OF_FRAMES", Value: strconv.FormatInt(s.frames, 10)},
			{Name: "NUMBER_OF_BYTES", Value: strconv.FormatInt(s.bytes, 10)},
		}
		if dur := s.durationMs(); dur > 0 {
			names = append([]string{"BPS", "DURATION"}, names...)
			simple = append([]mkv.SimpleTag{
				{Name: "BPS", Value: strconv.FormatInt(bitsPerSecond(s.bytes, dur), 10)},
				{Name: "DURATION", Value: formatStatsDuration(dur)},
			}, simple...)
		}
		// The conventional markers that say these values are auto-generated and
		// by whom. Without them a consumer keying on _STATISTICS_TAGS treats the
		// set as hand-written. No date is stamped: mkvgo's outputs stay
		// reproducible, and a wall clock would make two identical runs differ.
		simple = append(simple,
			mkv.SimpleTag{Name: "_STATISTICS_TAGS", Value: strings.Join(names, " ")},
			mkv.SimpleTag{Name: "_STATISTICS_WRITING_APP", Value: "mkvgo"})
		out = append(out, mkv.Tag{TargetID: uid, SimpleTags: simple})
	}
	return out
}

// formatStatsDuration renders a duration the way the statistics-tag convention
// do: HH:MM:SS.nnnnnnnnn.
// bitsPerSecond is bytes*8000/durationMs without the overflow that wrote a
// NEGATIVE bitrate into the file: the product passes 2^63 at about 1.15 PB, so
// beyond that the division comes first - a rounding no real file can notice, and
// a rate that stays a rate.
func bitsPerSecond(bytes, durationMs int64) int64 {
	if durationMs <= 0 || bytes <= 0 {
		return 0
	}
	if bytes <= math.MaxInt64/8000 {
		return bytes * 8 * 1000 / durationMs
	}
	// Past ~1.15 PB the product no longer fits: divide first, and saturate
	// rather than wrap - dividing first is not enough on its own, the result
	// can still pass 2^63.
	perMs := bytes / durationMs
	if perMs > math.MaxInt64/8000 {
		return math.MaxInt64
	}
	return perMs * 8000
}

func formatStatsDuration(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d.%09d",
		ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000*1_000_000)
}

func buildMuxTracks(ctx context.Context, inputs []mkv.TrackInput, fs *mkv.FS) ([]mkv.Track, map[trackKey]uint64, error) {
	var tracks []mkv.Track
	trackMap := make(map[trackKey]uint64)
	nextID := uint64(1)

	for _, inp := range inputs {
		c, err := reader.OpenWithFS(ctx, inp.SourcePath, fs)
		if err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", inp.SourcePath, err)
		}
		var srcTrack *mkv.Track
		for i := range c.Tracks {
			if c.Tracks[i].ID == inp.TrackID {
				srcTrack = &c.Tracks[i]
				break
			}
		}
		if srcTrack == nil {
			return nil, nil, fmt.Errorf("track %d not found in %s", inp.TrackID, inp.SourcePath)
		}

		t := *srcTrack
		t.ID = nextID
		if inp.Language != "" {
			t.Language = inp.Language
		}
		if inp.Name != "" {
			t.Name = inp.Name
		}
		t.IsDefault = inp.IsDefault

		trackMap[trackKey{inp.SourcePath, inp.TrackID}] = nextID
		tracks = append(tracks, t)
		nextID++
	}
	return tracks, trackMap, nil
}

type trackKey struct {
	path    string
	trackID uint64
}

// buildMuxSources groups the track inputs by source file (each becomes one
// mergeSource with a track remap) and returns them along with the output
// TimecodeScale and duration -- read from source metadata, so no blocks are
// loaded. streamMergeToWriter then merges them with bounded memory.
func buildMuxSources(ctx context.Context, inputs []mkv.TrackInput, trackMap map[trackKey]uint64, fs *mkv.FS) ([]mergeSource, int64, int64, error) {
	order := make([]string, 0, len(inputs))
	remaps := make(map[string]map[uint64]uint64)
	for _, inp := range inputs {
		rm, ok := remaps[inp.SourcePath]
		if !ok {
			rm = make(map[uint64]uint64)
			remaps[inp.SourcePath] = rm
			order = append(order, inp.SourcePath)
		}
		rm[inp.TrackID] = trackMap[trackKey{inp.SourcePath, inp.TrackID}]
	}

	var timecodeScale, durationMs int64
	sources := make([]mergeSource, 0, len(order))
	for _, path := range order {
		c, err := reader.OpenWithFS(ctx, path, fs)
		if err != nil {
			return nil, 0, 0, err
		}
		if timecodeScale == 0 {
			timecodeScale = c.Info.TimecodeScale
		}
		if c.DurationMs > durationMs {
			durationMs = c.DurationMs
		}
		sources = append(sources, mergeSource{path: path, scale: c.Info.TimecodeScale, remap: remaps[path]})
	}
	if timecodeScale == 0 {
		timecodeScale = 1000000
	}
	return sources, timecodeScale, durationMs, nil
}
