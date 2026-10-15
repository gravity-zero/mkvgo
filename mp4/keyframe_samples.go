package mp4

import (
	"context"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/mkv"
)

// SyncSample locates one sync sample of a video track: its bytes in the file
// and its presentation time (edit list applied, as Container.Keyframes).
type SyncSample struct {
	Offset, Size int64
	PtsMs        int64
}

// VideoSyncSamples reads the movie header of the MP4 in r and returns its first
// video track as Matroska metadata (codec, codec private data, dimensions,
// colour) and that track's sync samples in presentation order - the MP4
// counterpart of a Matroska video track's Cues, with each sample's exact byte
// range. A fragmented file's samples are read from its fragments. track is nil
// when the movie has no video track.
func VideoSyncSamples(ctx context.Context, r io.ReadSeeker) (track *mkv.Track, samples []SyncSample, err error) {
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
	mv, err := parseMP4(r, size, sampleFull)
	if err != nil {
		return nil, nil, err
	}
	tracks := buildMKVTracks(mv, false)
	for i := range mv.tracks {
		t := &mv.tracks[i]
		if t.trackType != mkv.VideoTrack || t.unsupported != "" || t.encryption != "" {
			continue
		}
		for _, s := range t.samples {
			if s.sync {
				samples = append(samples, SyncSample{Offset: s.offset, Size: int64(s.size), PtsMs: s.ctsMs})
			}
		}
		sort.SliceStable(samples, func(a, b int) bool { return samples[a].PtsMs < samples[b].PtsMs })
		vt := tracks[i]
		return &vt, samples, nil
	}
	return nil, nil, nil
}
