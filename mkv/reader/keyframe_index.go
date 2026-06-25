package reader

import (
	"bufio"
	"fmt"
	"io"
	"sort"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// keyframe_index.go — a COMPLETE keyframe index for a Cues-less Matroska: every
// video keyframe, equal to `ffprobe -skip_frame nokey`, not a sample. It is a
// single sequential structural pass over the Segment — cluster by cluster, never
// a per-block seek — reading only element headers and skipping block payloads by
// their declared size (no demux, no decode). The complementary low-cost variant
// is WithSampledKeyframes; this is WithKeyframeIndex.
//
// Block payloads are skipped, not read, so the pass transfers headers only. The
// bufReadSeeker discards a small payload from its buffer (staying sequential) and
// seeks past a large one (avoiding the transfer), so the cost is O(file size) in
// traversal but a fraction of it in bytes.

// buildKeyframeIndex returns every video keyframe's presentation time (ms),
// ascending and de-duplicated, from a single forward pass. On a header that will
// not decode (corruption) it resyncs to the next Cluster, bounded by the segment
// end, and continues — so a damaged region costs at most the keyframes in it.
// Matroska carries no edit list, so block times need no shift; this matches the
// Cues-derived index (keyframeTimesMs), the other Matroska keyframe path.
func (p *parser) buildKeyframeIndex(videoTrack uint64, scale int64) []int64 {
	segStart, segEnd := p.segStart, p.segEnd
	if segEnd < 0 { // unknown-size Segment: walk to the file end
		if end, err := p.r.Seek(0, io.SeekEnd); err == nil {
			segEnd = end
		}
	}
	if _, err := p.r.Seek(segStart, io.SeekStart); err != nil {
		return nil
	}
	// Read the Segment sequentially with read-ahead, discarding block payloads
	// in-stream rather than seeking past them: a seek-per-block walk pays a
	// round-trip per block and collapses on SMB/NAS latency (~67k blocks → minutes),
	// while a streaming read transfers the data in a few large reads, like ffprobe,
	// but pure-Go with no demux/decode. Existing helpers (readHeader/skip/…) are
	// reused unchanged; only the reader under them changes for the pass.
	raw, err := rawSeeker(p.r)
	if err != nil {
		return nil
	}
	prev := p.r
	p.r = newSeqSkipReader(raw, segStart)
	defer func() { p.r = prev }()
	if scale <= 0 {
		scale = 1_000_000
	}

	var times []int64
	var clusterTS int64
	var inCluster, haveTS bool
	clusterEnd := int64(-1)

	for inCluster || segEnd < 0 || p.pos() < segEnd {
		if inCluster && clusterEnd >= 0 && p.pos() >= clusterEnd {
			inCluster, haveTS, clusterEnd = false, false, -1
			continue
		}

		h, _, err := p.readHeader()
		if err != nil {
			if !p.resyncCluster(segEnd, &inCluster, &haveTS, &clusterTS, &clusterEnd) {
				break
			}
			continue
		}

		// An unknown-size Cluster ends at the next segment-level element; fall
		// through to handle that element at the top level.
		if inCluster && clusterEnd < 0 && isSegmentLevelID(h.ID) {
			inCluster, haveTS = false, false
		}

		if !inCluster {
			if h.ID == mkv.IDCluster {
				inCluster, haveTS, clusterTS = true, false, 0
				if h.Size >= 0 {
					clusterEnd = p.pos() + h.Size
				} else {
					clusterEnd = -1
				}
				continue
			}
			if h.Size < 0 {
				break
			}
			if err := p.skip(h.Size); err != nil {
				break
			}
			continue
		}

		// Inside a Cluster.
		switch h.ID {
		case mkv.IDTimestamp:
			v, err := ebml.ReadUint(p.r, h.Size)
			if err != nil {
				if !p.resyncCluster(segEnd, &inCluster, &haveTS, &clusterTS, &clusterEnd) {
					return finalizeIndex(times)
				}
				continue
			}
			clusterTS, haveTS = int64(v), true
		case mkv.IDSimpleBlock:
			rel, track, key, ok := p.peekBlockHeader(h.Size, true)
			if !ok {
				if !p.resyncCluster(segEnd, &inCluster, &haveTS, &clusterTS, &clusterEnd) {
					return finalizeIndex(times)
				}
				continue
			}
			if haveTS && key && (videoTrack == 0 || track == videoTrack) {
				times = append(times, scaleTimeMs(clusterTS+int64(rel), scale))
			}
		case mkv.IDBlockGroup:
			rel, track, key, ok := p.blockGroupKeyframe(h.Size)
			if !ok {
				if !p.resyncCluster(segEnd, &inCluster, &haveTS, &clusterTS, &clusterEnd) {
					return finalizeIndex(times)
				}
				continue
			}
			if haveTS && key && (videoTrack == 0 || track == videoTrack) {
				times = append(times, scaleTimeMs(clusterTS+int64(rel), scale))
			}
		default:
			if h.Size < 0 {
				break
			}
			if err := p.skip(h.Size); err != nil {
				break
			}
		}
	}
	return finalizeIndex(times)
}

// resyncCluster recovers from a corrupt header by scanning to the next valid
// Cluster and re-entering it. It updates the walk state in place and reports
// whether a Cluster was found (false → the caller stops the pass).
func (p *parser) resyncCluster(segEnd int64, inCluster, haveTS *bool, clusterTS, clusterEnd *int64) bool {
	off, err := p.resyncToCluster(segEnd)
	if err != nil || off < 0 {
		return false
	}
	h, _, err := p.readHeader()
	if err != nil || h.ID != mkv.IDCluster {
		return false
	}
	*inCluster, *haveTS, *clusterTS = true, false, 0
	if h.Size >= 0 {
		*clusterEnd = p.pos() + h.Size
	} else {
		*clusterEnd = -1
	}
	return true
}

// keyframeScanBufSize is the read-ahead window for the sequential keyframe pass.
// It must sit comfortably above the mount's read size (CIFS rsize is ~1–4 MiB) so
// each underlying read is large and the kernel read-ahead amortises per-read
// latency — at 256 KiB a 1.25 GB Segment costs ~5000 SMB reads (latency-bound);
// at 8 MiB it is a few hundred, in ffprobe's range.
const keyframeScanBufSize = 8 << 20

// discardChunk caps a single bufio.Discard call so the int conversion is safe on
// a 32-bit int platform; bufio refills across the cap on its own.
const discardChunk = 1 << 30

// rawSeeker returns the underlying seekable reader positioned where the buffered
// reader currently is, so the sequential pass can read from there with its own
// large buffer instead of through the small metadata buffer.
func rawSeeker(r io.ReadSeeker) (io.ReadSeeker, error) {
	if b, ok := r.(*bufReadSeeker); ok {
		return b.raw()
	}
	return r, nil
}

// seqSkipReader reads an io.ReadSeeker sequentially behind a large read-ahead
// buffer, serving a forward skip (a positive SeekCurrent, or a forward SeekStart)
// by DISCARDING bytes through the buffer rather than seeking past them. That keeps
// the underlying reads sequential: on SMB/NAS a seek-per-block walk pays a
// round-trip per block, whereas a streaming read with read-ahead transfers the
// data in a few large reads. A backward or current-position seek (the rare resync
// path, and position queries) is served directly.
type seqSkipReader struct {
	rs  io.ReadSeeker
	br  *bufio.Reader
	off int64
}

func newSeqSkipReader(rs io.ReadSeeker, startOff int64) *seqSkipReader {
	return &seqSkipReader{rs: rs, br: bufio.NewReaderSize(rs, keyframeScanBufSize), off: startOff}
}

func (s *seqSkipReader) Read(p []byte) (int, error) {
	n, err := s.br.Read(p)
	s.off += int64(n)
	return n, err
}

func (s *seqSkipReader) discard(n int64) (int64, error) {
	// bufio.Reader.Discard advances past n bytes WITHOUT copying them to a
	// destination (unlike io.CopyN, which streams through an 8 KiB scratch buffer),
	// refilling from rs in buffer-sized reads.
	for n > 0 {
		chunk := n
		if chunk > discardChunk {
			chunk = discardChunk
		}
		m, err := s.br.Discard(int(chunk))
		s.off += int64(m)
		n -= int64(m)
		if err != nil {
			return s.off, err
		}
	}
	return s.off, nil
}

func (s *seqSkipReader) seekAbs(target int64) (int64, error) {
	if _, err := s.rs.Seek(target, io.SeekStart); err != nil {
		return s.off, err
	}
	s.br.Reset(s.rs)
	s.off = target
	return target, nil
}

func (s *seqSkipReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		switch {
		case offset == 0:
			return s.off, nil
		case offset > 0:
			return s.discard(offset)
		default:
			return s.seekAbs(s.off + offset)
		}
	case io.SeekStart:
		switch {
		case offset == s.off:
			return s.off, nil
		case offset > s.off:
			return s.discard(offset - s.off)
		default:
			return s.seekAbs(offset)
		}
	case io.SeekEnd:
		t, err := s.rs.Seek(offset, io.SeekEnd)
		if err != nil {
			return s.off, err
		}
		s.br.Reset(s.rs)
		s.off = t
		return t, nil
	}
	return s.off, fmt.Errorf("seqSkipReader: invalid whence %d", whence)
}

// finalizeIndex sorts and de-duplicates the collected keyframe times.
func finalizeIndex(times []int64) []int64 {
	if len(times) == 0 {
		return nil
	}
	sort.Slice(times, func(a, b int) bool { return times[a] < times[b] })
	out := times[:1]
	for _, t := range times[1:] {
		if t != out[len(out)-1] {
			out = append(out, t)
		}
	}
	return out
}
