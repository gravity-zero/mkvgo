package mp4

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
)

// meta.go — metadata-only probe of an MP4 file. OpenMeta/ReadMeta parse only the
// movie header (the moov box: track sample entries, colour code points and
// chapters) and return the equivalent Matroska metadata WITHOUT reading any
// sample data (mdat) or writing an output file. This is the fast path for
// indexing or scanning an MP4 library — the counterpart of the mkv reader's
// OpenMeta/ReadMeta. Use RemuxFromMP4 to convert the media itself.

// OpenMeta opens the MP4 file at path and returns its metadata as a Container:
// Info, Tracks, Chapters and DurationMs are populated; Attachments, Tags and
// Cues are left nil (MP4 carries no equivalent of the first two, and Cues are
// not built on the metadata path). Only the moov box's headers are read — never
// the mdat sample data, and never the per-sample tables unless keyframes are
// requested — so it is fast and bounded regardless of file size.
//
// By default the keyframe index is NOT built: that requires expanding the full
// sample table, which dominates the parse cost on a long movie. Pass
// Options{Keyframes: true} to populate Container.Keyframes in the same pass.
//
// The second return value lists tracks present in the file but not represented in
// Container.Tracks — cover art / attached pictures and other non-media tracks
// (hint, timecode, metadata). It is nil when every track was carried. Surfacing
// them lets a probe report, for instance, that a file ffprobe counts as having two
// video streams has one playable video track plus a cover image.
func OpenMeta(ctx context.Context, path string, opts ...Options) (*mkv.Container, []DroppedTrack, error) {
	o := optionsFrom(opts)
	src, err := o.FS.DoOpen(path)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()
	return readMeta(ctx, src, path, o)
}

// OpenMetaWithFS is OpenMeta against a caller-provided FS (nil = the real OS FS).
func OpenMetaWithFS(ctx context.Context, path string, fs *mkv.FS, opts ...Options) (*mkv.Container, []DroppedTrack, error) {
	o := optionsFrom(opts)
	o.FS = fs
	return OpenMeta(ctx, path, o)
}

// ReadMeta reads only the MP4 movie header from r and returns the equivalent
// Matroska metadata (Info, Tracks, Chapters, DurationMs) plus the dropped (non-
// carried) tracks. It is the seekable, FS-free counterpart of OpenMeta; r must
// support seeking (the moov box may sit after the media). No sample data is read.
// Pass Options{Keyframes: true} to also populate Container.Keyframes.
func ReadMeta(ctx context.Context, r io.ReadSeeker, path string, opts ...Options) (*mkv.Container, []DroppedTrack, error) {
	return readMeta(ctx, r, path, optionsFrom(opts))
}

func readMeta(ctx context.Context, r io.ReadSeeker, path string, o Options) (*mkv.Container, []DroppedTrack, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	mode := sampleNone
	if o.Keyframes {
		mode = sampleKeyframes
	}
	mv, err := parseMP4(r, size, mode)
	if err != nil {
		return nil, nil, err
	}
	c := containerFromMovie(mv)
	c.Path = path
	if o.InBandColour {
		fillInBandColour(r, mv, c)
	}
	return c, mv.dropped, nil
}

// containerFromMovie assembles the Matroska metadata for a parsed movie. It is
// the single source of truth shared by ReadMeta (which returns it) and writeMKV
// (which writes it), so a probe and a full remux report identical metadata.
func containerFromMovie(mv *movie) *mkv.Container {
	const scale = 1_000_000 // 1 ms per timecode unit
	// Prefer the sample-derived duration when the sample table was built; otherwise
	// (metadata-only probe) use the mvhd movie duration.
	durMs := movieDurationMs(mv)
	if durMs == 0 {
		durMs = mv.durationMs
	}
	// The Nero chpl box carries no end times; parseChpl closes each chapter at the
	// next one's start and leaves the last open. Close the last at the movie end so
	// an explicit final ChapterTimeEnd survives the round trip instead of reading 0.
	if n := len(mv.chapters); n > 0 && mv.chapters[n-1].EndMs == 0 && durMs > mv.chapters[n-1].StartMs {
		mv.chapters[n-1].EndMs = durMs
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: scale,
			Title:         mv.title,
			MuxingApp:     "mkvgo",
			WritingApp:    "mkvgo",
			Duration:      float64(durMs),
		},
		Tracks:     buildMKVTracks(mv),
		Chapters:   mv.chapters,
		DurationMs: durMs,
		Keyframes:  videoKeyframesMs(mv),
		Fragmented: mv.fragmented,
	}
	if len(mv.tags) > 0 {
		c.Tags = []mkv.Tag{{SimpleTags: mv.tags}}
	}
	return c
}

// videoKeyframesMs returns the first video track's keyframe presentation
// timestamps (ms), ascending and de-duplicated, from the sync samples already
// parsed into the sample table — so the keyframe index costs nothing beyond the
// metadata parse OpenMeta already does. nil when there is no video track.
func videoKeyframesMs(mv *movie) []int64 {
	for i := range mv.tracks {
		t := &mv.tracks[i]
		if t.trackType != mkv.VideoTrack {
			continue
		}
		// sampleKeyframes mode already computed the times without offsets.
		if t.keyframesMs != nil {
			return t.keyframesMs
		}
		// Full-table path (remux): derive from the built sample table.
		times := make([]int64, 0, len(t.samples)/8+1)
		for j := range t.samples {
			// ctsMs already carries the edit-list shift (applied in buildSampleTable).
			if t.samples[j].sync {
				times = append(times, t.samples[j].ctsMs)
			}
		}
		return sortDedupTimes(times)
	}
	return nil
}
