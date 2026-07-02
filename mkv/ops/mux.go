package ops

import (
	"context"
	"fmt"
	"strconv"

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
	// mkvmerge-style per-track statistics accumulated during the stream. The
	// SeekHead points to it, so head-only readers (WithBitrate) still find it.
	tags := append(append([]mkv.Tag{}, opts.Tags...), statsTags(tracks, stats)...)
	if err := mw.WriteTagsElement(tags); err != nil {
		return err
	}
	return mw.Finalize()
}

// statsTags builds mkvmerge-style per-track statistics tags (BPS, DURATION,
// NUMBER_OF_FRAMES, NUMBER_OF_BYTES), keyed by track UID — the values ffprobe
// surfaces as TAG:BPS etc. and matroska.WithBitrate reads back head-only.
func statsTags(tracks []mkv.Track, stats map[uint64]*trackStats) []mkv.Tag {
	var out []mkv.Tag
	for _, t := range tracks {
		s := stats[t.ID]
		if s == nil || !s.seen {
			continue
		}
		dur := s.durationMs()
		if dur <= 0 {
			continue
		}
		uid := t.UID
		if uid == 0 {
			uid = t.ID // the writer defaults a zero UID to the track ID
		}
		out = append(out, mkv.Tag{
			TargetID: uid,
			SimpleTags: []mkv.SimpleTag{
				{Name: "BPS", Value: strconv.FormatInt(s.bytes*8*1000/dur, 10)},
				{Name: "DURATION", Value: formatStatsDuration(dur)},
				{Name: "NUMBER_OF_FRAMES", Value: strconv.FormatInt(s.frames, 10)},
				{Name: "NUMBER_OF_BYTES", Value: strconv.FormatInt(s.bytes, 10)},
			},
		})
	}
	return out
}

// formatStatsDuration renders a duration the way mkvmerge's statistics tags
// do: HH:MM:SS.nnnnnnnnn.
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
