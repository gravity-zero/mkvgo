package reader

import (
	"fmt"
	"io"
	"math/bits"
)

// readLaceHeader reads a lace header (after its count byte) from r and returns
// each frame's size and the header's length, the last frame sized from
// dataLen - the bytes the header and frames take together - as
// decodeLacingSizes does from the whole payload.
func readLaceHeader(r io.ByteReader, lacing byte, frameCount int, dataLen int64) (sizes []int, headerLen int, err error) {
	if lacing == lacingFixed {
		if dataLen%int64(frameCount) != 0 {
			return nil, 0, fmt.Errorf("fixed lacing: %d data bytes not divisible by %d frames", dataLen, frameCount)
		}
		sizes = make([]int, frameCount)
		for i := range sizes {
			sizes[i] = int(dataLen / int64(frameCount))
		}
		return sizes, 0, nil
	}
	// The header is gathered byte by byte - a Xiph size ends at its first byte
	// under 255, an EBML VINT's width is in its first byte - then decoded as
	// decodeLacingSizes decodes a whole payload, which sizes the last frame.
	var hdr []byte
	next := func() error {
		if int64(len(hdr)) >= dataLen {
			return fmt.Errorf("lace header overruns its %d data bytes", dataLen)
		}
		b, err := r.ReadByte()
		hdr = append(hdr, b)
		return err
	}
	for i := 0; i < frameCount-1; i++ {
		switch lacing {
		case lacingXiph:
			for {
				if err := next(); err != nil {
					return nil, 0, err
				}
				if hdr[len(hdr)-1] < 255 {
					break
				}
			}
		case lacingEBML:
			if err := next(); err != nil {
				return nil, 0, err
			}
			first := hdr[len(hdr)-1]
			if first == 0 {
				return nil, 0, fmt.Errorf("ebml lacing: invalid size vint at frame %d", i)
			}
			for w := bits.LeadingZeros8(first); w > 0; w-- {
				if err := next(); err != nil {
					return nil, 0, err
				}
			}
		default:
			return nil, 0, fmt.Errorf("unknown lacing type %d", lacing)
		}
	}
	// decodeLacingSizes sizes the last frame from the payload's length: hand it the header in front of a stand-in for the frames' length.
	sizes, headerLen, err = decodeLacingSizesLen(lacing, hdr, frameCount, dataLen)
	return sizes, headerLen, err
}

// decodeLacingSizes reads the per-frame sizes from a laced block's lacing
// header and returns them with the exact number of header bytes consumed -
// parseBlock slices the frame data right after them. Returning the consumed
// length (instead of re-deriving it from the sizes) stays correct even when a
// muxer encoded a size or diff in a wider-than-minimal VINT.
func decodeLacingSizes(lacing byte, raw []byte, frameCount int) (sizes []int, headerLen int, err error) {
	return decodeLacingSizesLen(lacing, raw, frameCount, int64(len(raw)))
}

// decodeLacingSizesLen decodes a lace header from raw, the last frame sized from total, the payload's length (raw may hold only the header).
func decodeLacingSizesLen(lacing byte, raw []byte, frameCount int, total64 int64) (sizes []int, headerLen int, err error) {
	n := int(total64)
	sizes = make([]int, frameCount)
	switch lacing {
	case lacingXiph:
		pos := 0
		total := 0
		for i := 0; i < frameCount-1; i++ {
			sz := 0
			for pos < len(raw) {
				val := raw[pos]
				pos++
				sz += int(val)
				if val < 255 {
					break
				}
			}
			sizes[i] = sz
			total += sz
		}
		last := n - pos - total
		if last < 0 {
			return nil, 0, fmt.Errorf("xiph lacing: total %d exceeds data %d", total, n-pos)
		}
		sizes[frameCount-1] = last
		return sizes, pos, nil
	case lacingFixed:
		if n%frameCount != 0 {
			return nil, 0, fmt.Errorf("fixed lacing: %d data bytes not divisible by %d frames", n, frameCount)
		}
		sz := n / frameCount
		for i := range sizes {
			sizes[i] = sz
		}
		return sizes, 0, nil
	case lacingEBML:
		if frameCount == 1 {
			sizes[0] = n // only the last frame's size goes uncoded: a lace of one codes none
			return sizes, 0, nil
		}
		pos := 0
		first, width := readVINTFromBuf(raw[pos:])
		if width == 0 {
			return nil, 0, fmt.Errorf("ebml lacing: invalid first-size vint")
		}
		firstSize := int(first & ^(uint64(1) << uint(width*7)))
		if firstSize > n {
			return nil, 0, fmt.Errorf("ebml lacing: first frame of %d bytes runs over the data %d", firstSize, n)
		}
		sizes[0] = firstSize
		pos += width
		total := firstSize
		for i := 1; i < frameCount-1; i++ {
			if pos > len(raw) {
				return nil, 0, fmt.Errorf("ebml lacing: header truncated at frame %d", i)
			}
			val, w := readVINTFromBuf(raw[pos:])
			if w == 0 {
				return nil, 0, fmt.Errorf("ebml lacing: invalid size vint at frame %d", i)
			}
			pos += w
			// The diff is a signed VINT: value − (2^(7·w−1) − 1), per RFC 9559.
			// (An off-by-one bias - 2^(7·w-1) - shifted every frame boundary by
			// the frame index and corrupted all EBML-laced audio while keeping
			// the block's TOTAL intact, so only a decoder noticed.)
			dataBits := uint(w * 7)
			bias := int64(1)<<(dataBits-1) - 1
			stripped := int64(val & ^(uint64(1) << dataBits))
			diff := stripped - bias
			sizes[i] = sizes[i-1] + int(diff)
			if sizes[i] < 0 {
				return nil, 0, fmt.Errorf("ebml lacing: negative frame size at index %d", i)
			}
			if total += sizes[i]; sizes[i] > n || total > n {
				return nil, 0, fmt.Errorf("ebml lacing: frames past %d run over the data %d", i, n)
			}
		}
		last := n - pos - total
		if last < 0 {
			return nil, 0, fmt.Errorf("ebml lacing: total %d exceeds data %d", total, n-pos)
		}
		sizes[frameCount-1] = last
		return sizes, pos, nil
	}
	return nil, 0, fmt.Errorf("unknown lacing type %d", lacing)
}

// readVINTFromBuf decodes the VINT at buf[0], length marker kept: (0, 0) without a marker or with fewer bytes than it announces.
func readVINTFromBuf(buf []byte) (uint64, int) {
	if len(buf) == 0 {
		return 0, 0
	}
	b := buf[0]
	width := bits.LeadingZeros8(b) + 1
	if b == 0 || width > len(buf) {
		return 0, 0 // no length marker, or fewer bytes than it announces
	}
	val := uint64(b)
	for i := 1; i < width; i++ {
		val = (val << 8) | uint64(buf[i])
	}
	return val, width
}
