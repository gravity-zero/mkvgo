package reader

import (
	"encoding/binary"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// sampled_keyframes.go — a coarse keyframe index for a Cues-less Matroska. Some
// muxers ship files with no Cues seek index, so the head-only keyframe index is
// empty and a caller would fall back to an external probe. Sampling recovers a
// usable (coarse) index with a bounded number of seeks, never a block-by-block
// decode. Opt-in via WithSampledKeyframes.
//
// Each sampled position is a GUARANTEED video keyframe: a Cluster start is NOT
// assumed to be one (it is not, for muxers that don't align Clusters to
// keyframes), so within each Cluster the block headers are read — payloads
// skipped by element size — to find the first real video keyframe (SimpleBlock
// keyframe flag, or a BlockGroup with no ReferenceBlock) and emit its exact
// presentation time. A Cluster with no such keyframe is skipped (bounded).

const (
	// maxClustersPerSample bounds the forward walk when a sampled Cluster carries
	// no video keyframe: at most this many Clusters are inspected per sample.
	maxClustersPerSample = 8
	// maxClusterBlocks bounds the block-header scan within one Cluster; the first
	// keyframe sits near the Cluster start, so this is only a safety net.
	maxClusterBlocks = 1024
)

// sampleClusterKeyframes probes n evenly-spaced byte offsets across the Segment
// body: at each it resyncs forward to the next Cluster and reads the first video
// keyframe's presentation time. The returned timestamps (ascending ms,
// de-duplicated) form a coarse-but-valid keyframe index, bounded to about n
// seeks. On any I/O error, or when the segment range or scale is unusable, it
// returns what it gathered (possibly nil) so the caller can fall back.
func (p *parser) sampleClusterKeyframes(n int, timecodeScale int64, videoTrack uint64) []int64 {
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
		ms, ok := p.nextKeyframeFrom(segEnd, videoTrack, timecodeScale)
		if !ok {
			continue // no keyframe before the segment end from here
		}
		if _, dup := seen[ms]; dup {
			continue // a neighbouring probe resolved to the same keyframe
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

// nextKeyframeFrom resyncs to the next Cluster at or after the current position
// and returns the first video keyframe's time in ms. A Cluster with no video
// keyframe is skipped, up to maxClustersPerSample Clusters forward.
func (p *parser) nextKeyframeFrom(limit int64, videoTrack uint64, scale int64) (int64, bool) {
	for c := 0; c < maxClustersPerSample; c++ {
		off, err := p.resyncToCluster(limit)
		if err != nil || off < 0 {
			return 0, false
		}
		if ms, ok := p.clusterFirstKeyframeMs(limit, videoTrack, scale); ok {
			return ms, true
		}
		// No keyframe in this Cluster: resume scanning just past its magic.
		if _, err := p.r.Seek(off+int64(len(clusterMagic)), io.SeekStart); err != nil {
			return 0, false
		}
	}
	return 0, false
}

// clusterFirstKeyframeMs reads the Cluster at the current reader position (which
// must sit at the Cluster ID, where resyncToCluster leaves it) and returns the
// presentation time in ms of its first keyframe on the video track — the Cluster
// Timestamp plus that block's relative timecode. ok is false when the Cluster
// carries no such keyframe or on a malformed read.
func (p *parser) clusterFirstKeyframeMs(limit int64, videoTrack uint64, scale int64) (int64, bool) {
	h, _, err := p.readHeader()
	if err != nil || h.ID != mkv.IDCluster {
		return 0, false
	}
	end := p.pos() + h.Size
	if h.Size < 0 || (limit >= 0 && end > limit) {
		end = limit
	}
	var clusterTS int64
	haveTS := false
	for blocks := 0; blocks < maxClusterBlocks; {
		if end >= 0 && p.pos() >= end {
			break
		}
		ch, _, err := p.readHeader()
		if err != nil {
			break
		}
		switch ch.ID {
		case mkv.IDTimestamp:
			v, err := ebml.ReadUint(p.r, ch.Size)
			if err != nil {
				return 0, false
			}
			clusterTS, haveTS = int64(v), true
		case mkv.IDSimpleBlock:
			blocks++
			rel, track, key, ok := p.peekBlockHeader(ch.Size, true)
			if !ok {
				return 0, false
			}
			if haveTS && key && (videoTrack == 0 || track == videoTrack) {
				return scaleTimeMs(clusterTS+int64(rel), scale), true
			}
		case mkv.IDBlockGroup:
			blocks++
			rel, track, key, ok := p.blockGroupKeyframe(ch.Size)
			if !ok {
				return 0, false
			}
			if haveTS && key && (videoTrack == 0 || track == videoTrack) {
				return scaleTimeMs(clusterTS+int64(rel), scale), true
			}
		default:
			if ch.Size < 0 {
				return 0, false
			}
			if err := p.skip(ch.Size); err != nil {
				return 0, false
			}
		}
	}
	return 0, false
}

// peekBlockHeader reads a (Simple)Block header — track number, relative timecode,
// flags — then skips the remaining payload, leaving the reader at the next
// element. For a SimpleBlock the keyframe bit (0x80) is read; a Block inside a
// BlockGroup carries no usable keyframe bit, so simple=false reports key=false
// and the caller derives keyframe from the absence of a ReferenceBlock.
func (p *parser) peekBlockHeader(size int64, simple bool) (relTC int16, track uint64, key, ok bool) {
	start := p.pos()
	tn, _, err := ebml.ReadDataSize(p.r) // track number is a VINT-encoded data size
	if err != nil {
		return 0, 0, false, false
	}
	var hdr [3]byte // 2-byte relative timecode + 1-byte flags
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return 0, 0, false, false
	}
	relTC = int16(binary.BigEndian.Uint16(hdr[:2]))
	key = simple && hdr[2]&0x80 != 0
	consumed := p.pos() - start
	if size < consumed {
		return 0, 0, false, false
	}
	if err := p.skip(size - consumed); err != nil {
		return 0, 0, false, false
	}
	return relTC, uint64(tn), key, true
}

// blockGroupKeyframe walks a BlockGroup: it reads the contained Block's header
// and detects a ReferenceBlock. A BlockGroup is a keyframe iff it has a Block and
// no ReferenceBlock (the block depends on no other frame).
func (p *parser) blockGroupKeyframe(size int64) (relTC int16, track uint64, key, ok bool) {
	end := p.pos() + size
	var haveBlock, haveRef bool
	for p.pos() < end {
		ch, _, err := p.readHeader()
		if err != nil {
			return 0, 0, false, false
		}
		switch ch.ID {
		case mkv.IDBlock:
			r, tk, _, bok := p.peekBlockHeader(ch.Size, false)
			if !bok {
				return 0, 0, false, false
			}
			relTC, track, haveBlock = r, tk, true
		case mkv.IDReferenceBlock:
			haveRef = true
			if err := p.skip(ch.Size); err != nil {
				return 0, 0, false, false
			}
		default:
			if ch.Size < 0 {
				return 0, 0, false, false
			}
			if err := p.skip(ch.Size); err != nil {
				return 0, 0, false, false
			}
		}
	}
	if !haveBlock {
		return 0, 0, false, false
	}
	return relTC, track, !haveRef, true
}

// videoTrackID returns the TrackNumber of the first video track, or 0 when there
// is none — used to keep the sampled keyframes to the video stream (audio frames
// are all "keyframes" and would otherwise be emitted as seek points).
func videoTrackID(c *mkv.Container) uint64 {
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.VideoTrack {
			return c.Tracks[i].ID
		}
	}
	return 0
}

// scaleTimeMs converts a timecode (in TimecodeScale units) to ms, clamping a
// rare negative result (a small negative block relative timecode at time 0) to 0.
func scaleTimeMs(v, scale int64) int64 {
	ms := v * scale / 1_000_000
	if ms < 0 {
		return 0
	}
	return ms
}
