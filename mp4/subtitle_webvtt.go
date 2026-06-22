package mp4

import (
	"context"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

// subtitle_webvtt.go — extract an embedded MP4 text subtitle track to WebVTT,
// in-process, replacing an `ffmpeg -map 0:s:N -f webvtt` fork. tx3g (mov_text) and
// native wvtt samples are decoded to cue text; each sample's composition time and
// duration give the cue timing.

// defaultSubDurationMs bounds the last cue when its sample duration is unknown.
const defaultSubDurationMs = 3000

// ExtractSubtitleWebVTT extracts the subtitle track trackID (a Container.Tracks ID
// from OpenMeta) from the MP4 at srcPath and writes it as WebVTT to w. Only the
// movie header and the track's subtitle samples are read. tx3g and wvtt are
// supported; other (e.g. bitmap) subtitle entries are rejected.
func ExtractSubtitleWebVTT(ctx context.Context, srcPath string, trackID uint64, w io.Writer, opts ...Options) error {
	o := optionsFrom(opts)
	fs := o.FS

	src, err := fs.DoOpen(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := fs.DoStat(srcPath)
	if err != nil {
		return err
	}

	mv, err := parseMP4(src, fi.Size())
	if err != nil {
		return err
	}
	// buildMKVTracks assigns 1-based IDs in mv.tracks order, so trackID maps to
	// mv.tracks[trackID-1].
	idx := int(trackID) - 1
	if idx < 0 || idx >= len(mv.tracks) {
		return errf("subtitle track %d not found", trackID)
	}
	t := &mv.tracks[idx]
	if t.trackType != mkv.SubtitleTrack {
		return errf("track %d is not a subtitle track", trackID)
	}

	var cues []subtitle.Cue
	for i := range t.samples {
		if err := ctx.Err(); err != nil {
			return err
		}
		s := &t.samples[i]
		data, err := readSample(src, s.offset, s.size)
		if err != nil {
			return errf("read subtitle sample: %w", err)
		}
		text, ok := decodeSubtitleSample(t.codec, data)
		if !ok {
			continue // empty / gap-filler sample
		}
		end := int64(0)
		if s.durMs > 0 {
			end = s.ctsMs + s.durMs
		}
		cues = append(cues, subtitle.Cue{StartMs: s.ctsMs, EndMs: end, Text: string(text)})
	}
	subtitle.ResolveCueEnds(cues, defaultSubDurationMs)
	return subtitle.WriteWebVTT(w, cues)
}
