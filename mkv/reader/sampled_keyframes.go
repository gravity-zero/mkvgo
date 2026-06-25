package reader

import (
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// sampled_keyframes.go — a coarse keyframe index for a Cues-less Matroska. Some
// muxers ship files with no Cues seek index, so the head-only keyframe index is
// empty and a caller would fall back to an external probe. Sampling Cluster
// timestamps recovers a usable (coarse) index with a bounded number of seeks,
// never a block-by-block scan. Opt-in via WithSampledKeyframes.

// sampleClusterKeyframes probes n evenly-spaced byte offsets across the Segment
// body: at each it resyncs forward to the next valid Cluster and reads that
// Cluster's Timestamp. Every Cluster start is a real seek point, so the returned
// timestamps (ascending ms, de-duplicated) form a coarse-but-valid keyframe
// index. It is bounded to about n seeks. On any I/O error, or when the segment
// range or timecode scale is unusable, it returns what it gathered (possibly
// nil), leaving the caller to fall back.
func (p *parser) sampleClusterKeyframes(n int, timecodeScale int64) []int64 {
	if n <= 0 {
		n = defaultKeyframeSamples
	}
	if timecodeScale <= 0 {
		timecodeScale = 1_000_000
	}
	segStart, segEnd := p.segStart, p.segEnd
	if segEnd < 0 { // unknown-size Segment: probe up to the file end
		if end, err := p.r.Seek(0, io.SeekEnd); err == nil {
			segEnd = end
		}
	}
	if segEnd <= segStart {
		return nil
	}
	span := segEnd - segStart
	seen := make(map[int64]struct{}, n)
	var times []int64
	for i := 0; i < n; i++ {
		target := segStart + int64(i)*span/int64(n)
		if _, err := p.r.Seek(target, io.SeekStart); err != nil {
			break
		}
		off, err := p.resyncToCluster(segEnd)
		if err != nil {
			break
		}
		if off < 0 {
			break // no Cluster remains before the segment end
		}
		ts, ok := p.readClusterTimestamp(segEnd)
		if !ok {
			continue
		}
		ms := ts * timecodeScale / 1_000_000
		if _, dup := seen[ms]; dup {
			continue // a neighbouring probe landed in the same Cluster
		}
		seen[ms] = struct{}{}
		times = append(times, ms)
	}
	if len(times) == 0 {
		return nil
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	return times
}

// readClusterTimestamp reads the Timestamp of the Cluster at the current reader
// position (which must sit at the Cluster ID, where resyncToCluster leaves it).
// The Timestamp is the Cluster's first child per the Matroska spec; a few children
// are scanned defensively in case Void/CRC-32 precede it. The raw value is in
// TimecodeScale units; ok is false on a malformed/absent Timestamp.
func (p *parser) readClusterTimestamp(limit int64) (int64, bool) {
	h, _, err := p.readHeader()
	if err != nil || h.ID != mkv.IDCluster {
		return 0, false
	}
	end := p.pos() + h.Size
	if h.Size < 0 || (limit >= 0 && end > limit) {
		end = limit
	}
	for i := 0; i < 16; i++ {
		if end >= 0 && p.pos() >= end {
			return 0, false
		}
		ch, _, err := p.readHeader()
		if err != nil {
			return 0, false
		}
		if ch.ID == mkv.IDTimestamp {
			v, err := ebml.ReadUint(p.r, ch.Size)
			if err != nil {
				return 0, false
			}
			return int64(v), true
		}
		if ch.Size < 0 {
			return 0, false // unknown-size child before the Timestamp: give up
		}
		if err := p.skip(ch.Size); err != nil {
			return 0, false
		}
	}
	return 0, false
}
