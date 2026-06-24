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
