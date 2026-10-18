package mp4

import (
	"encoding/binary"
	"io"
	"sort"
)

// maxTopLevelBoxes bounds the top-level boxes mdatRanges walks: real files hold a handful, and each costs a read.
const maxTopLevelBoxes = 1 << 16

// mdatRanges returns the [start, end) byte ranges of the payloads of the file's top-level mdat boxes, in file order;
// a 64-bit size and a size of 0 (to the end of the file) are read as ISO/IEC 14496-12 has them.
func mdatRanges(r io.ReadSeeker, size int64) ([][2]int64, error) {
	var ranges [][2]int64
	var hdr [16]byte
	for off, n := int64(0), 0; off+8 <= size; n++ {
		if n == maxTopLevelBoxes {
			return nil, errf("the file holds more than %d top-level boxes: it is damaged or forged", maxTopLevelBoxes)
		}
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return nil, err
		}
		boxSize, hlen := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		switch boxSize {
		case 1:
			if off+16 > size {
				return nil, errf("box %q at %d is cut short in its 64-bit size", hdr[4:8], off)
			}
			if _, err := io.ReadFull(r, hdr[8:16]); err != nil {
				return nil, err
			}
			boxSize, hlen = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		case 0:
			boxSize = size - off
		}
		if boxSize < hlen || boxSize > size-off {
			return nil, errf("box %q at %d declares %d bytes where %d remain: the file is damaged", hdr[4:8], off, boxSize, size-off)
		}
		if string(hdr[4:8]) == "mdat" {
			ranges = append(ranges, [2]int64{off + hlen, off + boxSize})
		}
		off += boxSize
	}
	return ranges, nil
}

// inMdat reports whether [off, off+n) lies inside one of ranges, as mdatRanges returns them: in file order, so the
// one range that can hold off is found by binary search, never a walk over thousands of forged boxes per sample.
func inMdat(ranges [][2]int64, off, n int64) bool {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][1] > off })
	return i < len(ranges) && off >= ranges[i][0] && n >= 0 && off+n <= ranges[i][1]
}
