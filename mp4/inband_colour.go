package mp4

import (
	"encoding/binary"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// inbandReadCap bounds the in-band colour read: the parameter sets and the
// Alternative Transfer Characteristics SEI sit at the very start of the first
// access unit, so a window this size always covers them while a large first
// frame is never read in full.
const inbandReadCap = 64 << 10

// firstSampleLoc returns the file offset and size of a track's first sample,
// read head-only from stsz + stco/co64 (no full sample-table expansion). size is
// 0 when the boxes are absent or unusable.
func firstSampleLoc(stblBoxes []memBox) (offset int64, size uint32) {
	stsz, ok := findMemBox(stblBoxes, "stsz")
	if !ok || len(stsz.payload) < 16 {
		return 0, 0
	}
	if ss := binary.BigEndian.Uint32(stsz.payload[4:8]); ss != 0 {
		size = ss // fixed sample size
	} else {
		size = binary.BigEndian.Uint32(stsz.payload[12:16]) // first stsz entry
	}
	if b, ok := findMemBox(stblBoxes, "stco"); ok && len(b.payload) >= 12 {
		offset = int64(binary.BigEndian.Uint32(b.payload[8:12]))
	} else if b, ok := findMemBox(stblBoxes, "co64"); ok && len(b.payload) >= 16 {
		offset = int64(binary.BigEndian.Uint64(b.payload[8:16]))
	} else {
		return 0, 0
	}
	if size == 0 {
		return 0, 0
	}
	return offset, size
}

// headSampleCount is how many head samples a video track records for the scan type.
const headSampleCount = reader.InBandScanSamples

type sampleLoc struct {
	off  int64
	size uint32
}

// headSampleLocs returns where the first n samples of a track live, from the
// first entries of stsz, stsc and stco/co64. The head-only read holds the head
// of each table (its read-ahead covers far more than these entries) and zeroes
// the rest, and a zeroed entry ends the walk: fewer samples, never wrong ones.
func headSampleLocs(stblBoxes []memBox, n int) []sampleLoc {
	stsz, ok1 := findMemBox(stblBoxes, "stsz")
	stsc, ok2 := findMemBox(stblBoxes, "stsc")
	if !ok1 || !ok2 || len(stsz.payload) < 12 || len(stsc.payload) < 8 {
		return nil
	}
	chunkOff, entry := 8, 4
	co, ok := findMemBox(stblBoxes, "stco")
	if !ok {
		if co, ok = findMemBox(stblBoxes, "co64"); !ok {
			return nil
		}
		entry = 8
	}
	be := binary.BigEndian
	fixed, count := be.Uint32(stsz.payload[4:8]), int(be.Uint32(stsz.payload[8:12]))
	chunks := int(be.Uint32(co.payload[4:8]))
	runs := int(be.Uint32(stsc.payload[4:8]))
	sizeOf := func(i int) uint32 {
		if fixed != 0 {
			return fixed
		}
		if p := 12 + 4*i; p+4 <= len(stsz.payload) {
			return be.Uint32(stsz.payload[p:])
		}
		return 0
	}
	chunkAt := func(c int) int64 { // c counts from 0
		p := chunkOff + entry*c
		if c >= chunks || p+entry > len(co.payload) {
			return 0
		}
		if entry == 8 {
			return int64(be.Uint64(co.payload[p:]))
		}
		return int64(be.Uint32(co.payload[p:]))
	}
	perChunk := func(c int) int { // samples_per_chunk of the run holding chunk c (from 0)
		spc := 0
		for i := 0; i < runs; i++ {
			p := 8 + 12*i
			if p+12 > len(stsc.payload) {
				break
			}
			first := int(be.Uint32(stsc.payload[p:]))
			if first == 0 || first > c+1 {
				break
			}
			spc = int(be.Uint32(stsc.payload[p+4:]))
		}
		return spc
	}
	var out []sampleLoc
	for c, i := 0, 0; i < n && i < count; c++ {
		off, spc := chunkAt(c), perChunk(c)
		if off <= 0 || spc <= 0 {
			break
		}
		for j := 0; j < spc && i < n && i < count; j, i = j+1, i+1 {
			sz := sizeOf(i)
			if sz == 0 {
				return out
			}
			out = append(out, sampleLoc{off, sz})
			off += int64(sz)
		}
	}
	return out
}

// inbandScanBudget bounds the bytes the H.264 scan type reads over all the head
// samples, and inbandScanCap each sample: the SEI and the slice headers open the
// access unit.
const (
	inbandScanBudget = 1 << 20
	inbandScanCap    = 16 << 10
)

// scanFromSamples settles an H.264 track's scan type from its head samples:
// each run of contiguous samples is one read, at most inbandScanBudget in all,
// and each sample is read up to inbandScanCap.
func scanFromSamples(r io.ReadSeeker, locs []sampleLoc, mt *mkv.Track) {
	s := reader.NewInBandScan(mt)
	if s == nil {
		return
	}
	defer s.Apply(mt)
	budget := int64(inbandScanBudget)
	for i := 0; i < len(locs) && budget > 0; {
		j, end := i+1, locs[i].off+prefixLen(locs[i])
		for j < len(locs) && locs[j].off == locs[j-1].off+int64(locs[j-1].size) && end-locs[i].off < budget {
			end = locs[j].off + prefixLen(locs[j])
			j++
		}
		n := min(end-locs[i].off, budget)
		budget -= n
		if _, err := r.Seek(locs[i].off, io.SeekStart); err != nil {
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		for k := i; k < j; k++ {
			start := locs[k].off - locs[i].off
			if start >= n {
				return
			}
			stop := min(start+int64(locs[k].size), n, start+inbandScanCap)
			if s.Feed(buf[start:stop], stop-start < int64(locs[k].size)) {
				return
			}
		}
		i = j
	}
}

func prefixLen(l sampleLoc) int64 { return min(int64(l.size), inbandScanCap) }

// fillInBandColour is the Options.InBandColour worker for MP4. For each video
// track whose colour is still unknown and carries a bare hvcC, it reads a bounded
// window of that track's first sample and recovers the colour from the in-band
// HEVC SPS VUI (and ATC SEI), reusing the reader package's logic. Best-effort:
// any failure leaves the colour unset, exactly as without the option.
func fillInBandColour(r io.ReadSeeker, mv *movie, c *mkv.Container) {
	for i := range mv.tracks {
		if i >= len(c.Tracks) {
			break
		}
		t, mt := &mv.tracks[i], &c.Tracks[i]
		scanFromSamples(r, t.headSamples, mt)
		if t.firstSampleSize == 0 || !reader.NeedsInBandColour(mt) {
			continue
		}
		n := int64(t.firstSampleSize)
		if n > inbandReadCap {
			n = inbandReadCap
		}
		if _, err := r.Seek(t.firstSampleOffset, io.SeekStart); err != nil {
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			continue
		}
		reader.ApplyInBandColour(mt, buf)
	}
}
