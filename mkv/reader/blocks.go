package reader

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

const (
	lacingNone  = 0
	lacingXiph  = 1
	lacingFixed = 2
	lacingEBML  = 3

	progressInterval = 50
	maxBlockSize     = 64 * 1024 * 1024 // 64 MB max per block
)

type BlockReader struct {
	r             io.ReadSeeker
	segEnd        int64
	clusterEnd    int64
	clusterTS     int64
	timecodeScale int64
	pending       []mkv.Block
	progressFn    mkv.ProgressFunc
	progressTotal int64
	progressTick  int
}

func NewBlockReader(r io.ReadSeeker, timecodeScale int64) (*BlockReader, error) {
	br := &BlockReader{r: r, timecodeScale: timecodeScale, segEnd: -1, clusterEnd: -1}
	if err := br.init(); err != nil {
		return nil, err
	}
	return br, nil
}

func (br *BlockReader) SetProgress(fn mkv.ProgressFunc, total int64) {
	br.progressFn = fn
	br.progressTotal = total
}

func (br *BlockReader) reportProgress() {
	if br.progressFn == nil {
		return
	}
	pos, _ := br.r.Seek(0, io.SeekCurrent)
	br.progressFn(pos, br.progressTotal)
}

func (br *BlockReader) init() error {
	h, _, err := ebml.ReadElementHeader(br.r)
	if err != nil {
		return fmt.Errorf("read EBML header: %w", err)
	}
	if h.ID != ebml.IDEBMLHeader {
		return fmt.Errorf("expected EBML header, got 0x%X", h.ID)
	}
	if _, err := br.r.Seek(h.Size, io.SeekCurrent); err != nil {
		return err
	}
	h, _, err = ebml.ReadElementHeader(br.r)
	if err != nil {
		return fmt.Errorf("read segment: %w", err)
	}
	if h.ID != mkv.IDSegment {
		return fmt.Errorf("expected Segment, got 0x%X", h.ID)
	}
	if h.Size >= 0 {
		cur, _ := br.r.Seek(0, io.SeekCurrent)
		br.segEnd = cur + h.Size
	}
	return nil
}

func (br *BlockReader) Next() (mkv.Block, error) {
	if len(br.pending) > 0 {
		b := br.pending[0]
		br.pending = br.pending[1:]
		return b, nil
	}

	br.progressTick++
	if br.progressTick%progressInterval == 0 {
		br.reportProgress()
	}

	for {
		if br.clusterEnd > 0 {
			cur, _ := br.r.Seek(0, io.SeekCurrent)
			if cur >= br.clusterEnd {
				br.clusterEnd = -1
				continue
			}
			h, _, err := ebml.ReadElementHeader(br.r)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return mkv.Block{}, io.EOF
				}
				return mkv.Block{}, err
			}
			switch h.ID {
			case mkv.IDTimestamp:
				v, err := ebml.ReadUint(br.r, h.Size)
				if err != nil {
					return mkv.Block{}, err
				}
				br.clusterTS = int64(v)
				continue
			case mkv.IDSimpleBlock:
				return br.parseBlock(h.Size, true)
			case mkv.IDBlockGroup:
				return br.parseBlockGroup(h.Size)
			default:
				if _, err := br.r.Seek(h.Size, io.SeekCurrent); err != nil {
					return mkv.Block{}, err
				}
				continue
			}
		}

		if br.segEnd >= 0 {
			cur, _ := br.r.Seek(0, io.SeekCurrent)
			if cur >= br.segEnd {
				return mkv.Block{}, io.EOF
			}
		}

		h, _, err := ebml.ReadElementHeader(br.r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return mkv.Block{}, io.EOF
			}
			return mkv.Block{}, err
		}
		if h.ID == mkv.IDCluster {
			if h.Size >= 0 {
				cur, _ := br.r.Seek(0, io.SeekCurrent)
				br.clusterEnd = cur + h.Size
			}
			continue
		}
		if h.Size < 0 {
			return mkv.Block{}, fmt.Errorf("unknown-size element 0x%X outside cluster", h.ID)
		}
		if _, err := br.r.Seek(h.Size, io.SeekCurrent); err != nil {
			return mkv.Block{}, err
		}
	}
}

func (br *BlockReader) parseBlock(size int64, simple bool) (mkv.Block, error) {
	start, _ := br.r.Seek(0, io.SeekCurrent)

	trackNum, _, err := ebml.ReadDataSize(br.r)
	if err != nil {
		return mkv.Block{}, err
	}

	var tcBuf [2]byte
	if _, err := io.ReadFull(br.r, tcBuf[:]); err != nil {
		return mkv.Block{}, err
	}
	relTC := int16(binary.BigEndian.Uint16(tcBuf[:]))

	var flagsBuf [1]byte
	if _, err := io.ReadFull(br.r, flagsBuf[:]); err != nil {
		return mkv.Block{}, err
	}
	flags := flagsBuf[0]
	keyframe := simple && flags&0x80 != 0
	lacing := (flags >> 1) & 0x03

	cur, _ := br.r.Seek(0, io.SeekCurrent)
	dataSize := size - (cur - start)
	if dataSize < 0 || dataSize > maxBlockSize {
		return mkv.Block{}, fmt.Errorf("invalid block data size %d", dataSize)
	}

	if lacing == lacingNone {
		data := make([]byte, dataSize)
		if _, err := io.ReadFull(br.r, data); err != nil {
			return mkv.Block{}, err
		}
		tc, err := safeTimecodeMs(br.clusterTS+int64(relTC), br.timecodeScale)
		if err != nil {
			return mkv.Block{}, err
		}
		return mkv.Block{
			TrackNumber: uint64(trackNum), Timecode: tc,
			Keyframe: keyframe, Data: data,
		}, nil
	}

	raw := make([]byte, dataSize)
	if _, err := io.ReadFull(br.r, raw); err != nil {
		return mkv.Block{}, err
	}

	frameCount := int(raw[0]) + 1
	raw = raw[1:]

	frameSizes, err := decodeLacingSizes(lacing, raw, frameCount)
	if err != nil {
		return mkv.Block{}, fmt.Errorf("decode lacing: %w", err)
	}

	headerBytes := lacingHeaderLen(lacing, frameSizes)
	raw = raw[headerBytes:]

	tc, err := safeTimecodeMs(br.clusterTS+int64(relTC), br.timecodeScale)
	if err != nil {
		return mkv.Block{}, err
	}
	blocks := make([]mkv.Block, frameCount)
	offset := 0
	for i := 0; i < frameCount; i++ {
		end := offset + frameSizes[i]
		if end > len(raw) {
			return mkv.Block{}, fmt.Errorf("laced frame %d overflows: need %d, have %d", i, end, len(raw))
		}
		blocks[i] = mkv.Block{
			TrackNumber: uint64(trackNum), Timecode: tc,
			Keyframe: keyframe && i == 0, Data: append([]byte(nil), raw[offset:end]...),
		}
		offset = end
	}

	br.pending = blocks[1:]
	return blocks[0], nil
}

func (br *BlockReader) parseBlockGroup(size int64) (mkv.Block, error) {
	start, _ := br.r.Seek(0, io.SeekCurrent)
	end := start + size
	var block mkv.Block
	var found bool

	for {
		cur, _ := br.r.Seek(0, io.SeekCurrent)
		if cur >= end {
			break
		}
		h, _, err := ebml.ReadElementHeader(br.r)
		if err != nil {
			return mkv.Block{}, err
		}
		if h.ID == mkv.IDBlock {
			block, err = br.parseBlock(h.Size, false)
			if err != nil {
				return mkv.Block{}, err
			}
			found = true
		} else {
			if _, err := br.r.Seek(h.Size, io.SeekCurrent); err != nil {
				return mkv.Block{}, err
			}
		}
	}
	if !found {
		return mkv.Block{}, fmt.Errorf("BlockGroup without Block element")
	}
	return block, nil
}

func decodeLacingSizes(lacing byte, raw []byte, frameCount int) ([]int, error) {
	sizes := make([]int, frameCount)
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
		last := len(raw) - pos - total
		if last < 0 {
			return nil, fmt.Errorf("xiph lacing: total %d exceeds data %d", total, len(raw)-pos)
		}
		sizes[frameCount-1] = last
		return sizes, nil
	case lacingFixed:
		sz := len(raw) / frameCount
		for i := range sizes {
			sizes[i] = sz
		}
		return sizes, nil
	case lacingEBML:
		pos := 0
		first, width := readVINTFromBuf(raw[pos:])
		firstSize := int(first & ^(uint64(1) << uint(width*7)))
		sizes[0] = firstSize
		pos += width
		total := firstSize
		for i := 1; i < frameCount-1; i++ {
			val, w := readVINTFromBuf(raw[pos:])
			pos += w
			dataBits := uint(w * 7)
			bias := int64(1) << (dataBits - 1)
			stripped := int64(val & ^(uint64(1) << dataBits))
			diff := stripped - bias
			sizes[i] = sizes[i-1] + int(diff)
			if sizes[i] < 0 {
				return nil, fmt.Errorf("ebml lacing: negative frame size at index %d", i)
			}
			total += sizes[i]
		}
		last := len(raw) - pos - total
		if last < 0 {
			return nil, fmt.Errorf("ebml lacing: total %d exceeds data %d", total, len(raw)-pos)
		}
		sizes[frameCount-1] = last
		return sizes, nil
	}
	return nil, fmt.Errorf("unknown lacing type %d", lacing)
}

func lacingHeaderLen(lacing byte, sizes []int) int {
	switch lacing {
	case lacingXiph:
		n := 0
		for i := 0; i < len(sizes)-1; i++ {
			n += sizes[i]/255 + 1
		}
		return n
	case lacingFixed:
		return 0
	case lacingEBML:
		if len(sizes) == 0 {
			return 0
		}
		n := vintLen(uint64(sizes[0]))
		for i := 1; i < len(sizes)-1; i++ {
			n += signedVINTLen(sizes[i] - sizes[i-1])
		}
		return n
	}
	return 0
}

func readVINTFromBuf(buf []byte) (uint64, int) {
	if len(buf) == 0 {
		return 0, 0
	}
	b := buf[0]
	width := 1
	for i := 7; i >= 0; i-- {
		if b&(1<<uint(i)) != 0 {
			width = 8 - i
			break
		}
	}
	val := uint64(b)
	for i := 1; i < width && i < len(buf); i++ {
		val = (val << 8) | uint64(buf[i])
	}
	return val, width
}

func vintLen(val uint64) int {
	for w := 1; w <= 8; w++ {
		if val < (uint64(1)<<uint(w*7))-1 {
			return w
		}
	}
	return 8
}

func signedVINTLen(diff int) int {
	for w := 1; w <= 8; w++ {
		bias := int64(1) << uint(w*7-1)
		if int64(diff) >= -bias && int64(diff) < bias {
			return w
		}
	}
	return 8
}

func safeTimecodeMs(v, scale int64) (int64, error) {
	if scale != 0 && (v > math.MaxInt64/scale || v < math.MinInt64/scale) {
		return 0, fmt.Errorf("timecode overflow: %d * %d", v, scale)
	}
	return v * scale / 1_000_000, nil
}
