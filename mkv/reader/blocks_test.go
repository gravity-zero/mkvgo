package reader

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func buildBlockReaderInput(clusterPayload []byte) *bytes.Reader {
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(len(clusterPayload)))
	seg.Write(clusterPayload)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return bytes.NewReader(buf.Bytes())
}

func TestBlockNegativeDataSize(t *testing.T) {
	// SimpleBlock with trackNum=1, timecode=0, flags=0x80, but element size
	// is too small to hold even the header — resulting in negative dataSize.
	var cluster bytes.Buffer
	// timestamp = 0
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)
	// SimpleBlock with size=2 (too small: need >=4 for track+tc+flags)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, 2)
	cluster.Write([]byte{0x81, 0x00}) // track=1, partial timecode

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	_, err = br.Next()
	if err == nil {
		t.Fatal("expected error for block with insufficient data")
	}
}

func TestLacingFrameSizeExceedsData(t *testing.T) {
	// Build a SimpleBlock with Xiph lacing claiming more frames than data
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// SimpleBlock payload: track=1, tc=0, flags=lacing_xiph(0x02), frameCount=2, sizes exceed data
	blockPayload := []byte{
		0x81,       // track number 1
		0x00, 0x00, // timecode = 0
		0x02,       // flags: xiph lacing
		0x01,       // frame count = 2 (1+1)
		0xFF, 0xFF, // xiph size: 255+255 = 510 bytes for first frame
		0x00, // terminator
		0xAA, // only 1 byte of actual data
	}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	_, err = br.Next()
	if err == nil {
		t.Fatal("expected error for lacing frame exceeding data")
	}
	if !strings.Contains(err.Error(), "lac") && !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadVINTFromBuf(t *testing.T) {
	tests := []struct {
		name      string
		buf       []byte
		wantVal   uint64
		wantWidth int
	}{
		{"empty", nil, 0, 0},
		{"1-byte 0x81", []byte{0x81}, 0x81, 1},
		{"1-byte 0xFF", []byte{0xFF}, 0xFF, 1},
		{"2-byte", []byte{0x40, 0x01}, 0x4001, 2},
		{"4-byte", []byte{0x10, 0x00, 0x00, 0x01}, 0x10000001, 4},
		{"truncated 2-byte", []byte{0x40}, 0x40, 2}, // only 1 byte available for 2-byte VINT
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, width := readVINTFromBuf(tt.buf)
			if val != tt.wantVal || width != tt.wantWidth {
				t.Errorf("readVINTFromBuf(%x) = (%x, %d), want (%x, %d)", tt.buf, val, width, tt.wantVal, tt.wantWidth)
			}
		})
	}
}

func TestVintLen(t *testing.T) {
	tests := []struct {
		val  uint64
		want int
	}{
		{0, 1},
		{1, 1},
		{126, 1}, // max 1-byte: 2^7 - 2 = 126
		{127, 2}, // 2^7 - 1 triggers width 2
		{128, 2},
		{16382, 2}, // max 2-byte: 2^14 - 2
		{16383, 3},
		{math.MaxUint64, 8},
	}
	for _, tt := range tests {
		got := vintLen(tt.val)
		if got != tt.want {
			t.Errorf("vintLen(%d) = %d, want %d", tt.val, got, tt.want)
		}
	}
}

func TestSignedVINTLen(t *testing.T) {
	tests := []struct {
		diff int
		want int
	}{
		{0, 1},
		{1, 1},
		{-1, 1},
		{63, 1}, // bias for w=1 is 2^6 = 64, so 63 < 64 fits
		{-64, 1},
		{64, 2},
		{-65, 2},
		{8191, 2}, // bias for w=2 is 2^13 = 8192
		{-8192, 2},
		{8192, 3},
	}
	for _, tt := range tests {
		got := signedVINTLen(tt.diff)
		if got != tt.want {
			t.Errorf("signedVINTLen(%d) = %d, want %d", tt.diff, got, tt.want)
		}
	}
}

func TestSafeTimecodeMsOverflow(t *testing.T) {
	_, err := safeTimecodeMs(math.MaxInt64, 2)
	if err == nil {
		t.Fatal("expected overflow error")
	}
	if !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSafeTimecodeMsNormal(t *testing.T) {
	// 1000 * 1000000 / 1000000 = 1000 ms
	got, err := safeTimecodeMs(1000, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1000 {
		t.Errorf("got %d, want 1000", got)
	}
}

func TestSafeTimecodeMsZeroScale(t *testing.T) {
	got, err := safeTimecodeMs(500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestSafeTimecodeMsNegativeOverflow(t *testing.T) {
	_, err := safeTimecodeMs(math.MinInt64, 2)
	if err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestBlockGroupWithoutBlock(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// BlockGroup containing only a non-Block element
	var bg bytes.Buffer
	ebml.WriteElementHeader(&bg, mkv.IDBlockDuration, 1)
	ebml.WriteUint(&bg, 100, 1)

	ebml.WriteElementHeader(&cluster, mkv.IDBlockGroup, int64(bg.Len()))
	cluster.Write(bg.Bytes())

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	_, err = br.Next()
	if err == nil || !strings.Contains(err.Error(), "without Block") {
		t.Fatalf("expected 'without Block' error, got: %v", err)
	}
}

func TestBlockReaderProgress(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	data := []byte{0xDE, 0xAD}
	for i := 0; i < 60; i++ {
		blockPayload := []byte{0x81, 0x00, 0x00, 0x80}
		blockPayload = append(blockPayload, data...)
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
		cluster.Write(blockPayload)
	}

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	var called int
	br.SetProgress(func(processed, total int64) { called++ }, 1000)

	for {
		_, err := br.Next()
		if err != nil {
			break
		}
	}
	if called == 0 {
		t.Error("progress callback never called")
	}
}

func TestFixedLacing(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// SimpleBlock with fixed lacing: 3 frames of 2 bytes each
	blockPayload := []byte{
		0x81,       // track 1
		0x00, 0x00, // timecode 0
		0x04,       // flags: fixed lacing (0x04 = lacing bits = 10)
		0x02,       // frame count = 3 (2+1)
		0xAA, 0xBB, // frame 0
		0xCC, 0xDD, // frame 1
		0xEE, 0xFF, // frame 2
	}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	for i := 0; i < 3; i++ {
		b, err := br.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(b.Data) != 2 {
			t.Errorf("frame %d: got %d bytes, want 2", i, len(b.Data))
		}
	}
}

func TestEBMLLacing(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// Build a SimpleBlock with EBML lacing: 3 frames
	// Frame sizes: 100, 100, remaining
	// EBML lacing encodes: first size as VINT, subsequent diffs as signed VINTs
	frame0 := make([]byte, 100)
	frame1 := make([]byte, 100)
	frame2 := make([]byte, 50)
	for i := range frame0 {
		frame0[i] = 0xAA
	}
	for i := range frame1 {
		frame1[i] = 0xBB
	}
	for i := range frame2 {
		frame2[i] = 0xCC
	}

	// EBML lacing header: frameCount-1=2, first size=100 (VINT: 0xE4 = 0x80|100),
	// diff for frame 1 = 0 (signed VINT: 0xC0 = 0x40|32+32 = bias at center)
	var lacingData bytes.Buffer
	lacingData.WriteByte(0x02)        // frame count = 3 (2+1)
	lacingData.WriteByte(0x80 | 100)  // first size = 100 (1-byte VINT)
	lacingData.WriteByte(0x40 | 0x20) // diff = 0 (signed 2-byte VINT center... no)

	// Actually let me just manually construct the raw block data
	// track=1, tc=0, flags=EBML lacing (0x06), frameCount byte, VINT sizes, data
	blockPayload := []byte{
		0x81,       // track 1
		0x00, 0x00, // timecode 0
		0x06, // flags: EBML lacing (bits 1-0 = 11 = 3 = EBML)
		0x02, // frame count = 3 (2+1)
	}
	// First frame size = 100 -> VINT: 100 < 127, so 1-byte: 0x80 | 100 = 0xE4
	blockPayload = append(blockPayload, 0x80|100)
	// Diff for frame 1: 100 - 100 = 0, signed VINT with w=1: bias = 2^6 = 64
	// encoded = 0 + 64 = 64, with marker: 0x40 | 64 = 0x80... wait.
	// For w=1, data bits = 7, marker = 1<<6 = 0x40
	// stripped = diff + bias = 0 + 64 = 64
	// encoded = 0x40 | 64 = 0x40 | 0x40 = 0x80... that's a valid 1-byte VINT
	// Actually: the VINT value = marker | stripped = (1<<6) | 64 = 64 | 64 = 0x40 + 0x40
	// Hmm, let me reconsider. readVINTFromBuf returns (val, width).
	// For EBML lacing diff decoding:
	//   dataBits = w * 7
	//   bias = 1 << (dataBits - 1)
	//   stripped = val & ^(1 << dataBits)
	//   diff = stripped - bias
	// For diff=0, w=1: dataBits=7, bias=64
	//   stripped = 0 + 64 = 64 = 0x40
	//   val = (1<<7) | 64 = 128 + 64 = 192 = 0xC0
	blockPayload = append(blockPayload, 0xC0) // diff = 0

	blockPayload = append(blockPayload, frame0...)
	blockPayload = append(blockPayload, frame1...)
	blockPayload = append(blockPayload, frame2...)

	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	sizes := []int{100, 100, 50}
	vals := []byte{0xAA, 0xBB, 0xCC}
	for i := 0; i < 3; i++ {
		b, err := br.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if len(b.Data) != sizes[i] {
			t.Errorf("frame %d: got %d bytes, want %d", i, len(b.Data), sizes[i])
		}
		if len(b.Data) > 0 && b.Data[0] != vals[i] {
			t.Errorf("frame %d: first byte = 0x%X, want 0x%X", i, b.Data[0], vals[i])
		}
	}
}

func TestXiphLacing(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	frame0 := make([]byte, 300) // > 255, needs multiple xiph size bytes
	frame1 := make([]byte, 50)
	for i := range frame0 {
		frame0[i] = 0x11
	}
	for i := range frame1 {
		frame1[i] = 0x22
	}

	blockPayload := []byte{
		0x81,       // track 1
		0x00, 0x00, // timecode 0
		0x02,     // flags: xiph lacing
		0x01,     // frame count = 2 (1+1)
		0xFF, 45, // xiph size: 255 + 45 = 300
	}
	blockPayload = append(blockPayload, frame0...)
	blockPayload = append(blockPayload, frame1...)

	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	b0, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(b0.Data) != 300 {
		t.Errorf("frame 0 size = %d, want 300", len(b0.Data))
	}
	b1, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if len(b1.Data) != 50 {
		t.Errorf("frame 1 size = %d, want 50", len(b1.Data))
	}
}

func TestDecodeLacingSizesUnknownType(t *testing.T) {
	_, err := decodeLacingSizes(0xFF, []byte{0x01}, 2)
	if err == nil {
		t.Fatal("expected error for unknown lacing type")
	}
}

func TestLacingHeaderLen(t *testing.T) {
	// Xiph
	n := lacingHeaderLen(lacingXiph, []int{300, 50})
	// 300 = 255 + 45: needs 2 bytes
	if n != 2 {
		t.Errorf("xiph header len = %d, want 2", n)
	}

	// Fixed
	n = lacingHeaderLen(lacingFixed, []int{100, 100})
	if n != 0 {
		t.Errorf("fixed header len = %d, want 0", n)
	}

	// EBML: first size + diffs
	n = lacingHeaderLen(lacingEBML, []int{100, 100, 50})
	if n == 0 {
		t.Error("ebml header len should be > 0")
	}

	// EBML empty
	n = lacingHeaderLen(lacingEBML, nil)
	if n != 0 {
		t.Errorf("ebml empty header len = %d, want 0", n)
	}

	// Unknown
	n = lacingHeaderLen(0xFF, []int{100})
	if n != 0 {
		t.Errorf("unknown lacing header len = %d, want 0", n)
	}
}

func TestBlockReaderInitErrors(t *testing.T) {
	// Not an EBML header
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, mkv.IDCluster, 0)
	r := bytes.NewReader(buf.Bytes())
	_, err := NewBlockReader(r, 1000000)
	if err == nil {
		t.Fatal("expected error for non-EBML header")
	}
}

func TestBlockReaderNotSegment(t *testing.T) {
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDCluster, 0) // should be Segment
	r := bytes.NewReader(buf.Bytes())
	_, err := NewBlockReader(r, 1000000)
	if err == nil {
		t.Fatal("expected error for non-Segment")
	}
}

func TestBlockReaderEmptySegment(t *testing.T) {
	r := buildBlockReaderInput(nil)
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	_, err = br.Next()
	if err == nil {
		t.Fatal("expected EOF")
	}
}

func TestSignedVINTLenLargeValues(t *testing.T) {
	got := signedVINTLen(1 << 25)
	if got < 4 {
		t.Errorf("signedVINTLen(%d) = %d, expected >= 4", 1<<25, got)
	}
	got = signedVINTLen(-(1 << 25))
	if got < 4 {
		t.Errorf("signedVINTLen(%d) = %d, expected >= 4", -(1 << 25), got)
	}
	// Extremely large value that requires 8 bytes (exceeds all w=1..7 ranges)
	got = signedVINTLen(math.MaxInt64)
	if got != 8 {
		t.Errorf("signedVINTLen(MaxInt64) = %d, want 8", got)
	}
}

func TestTruncatedBlockReader(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// Normal block
	blockPayload := []byte{0x81, 0x00, 0x00, 0x80, 0xDE, 0xAD}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	// BlockGroup
	var bg bytes.Buffer
	innerBlock := []byte{0x81, 0x00, 0x00, 0x00, 0xBE, 0xEF}
	ebml.WriteElementHeader(&bg, mkv.IDBlock, int64(len(innerBlock)))
	bg.Write(innerBlock)
	ebml.WriteElementHeader(&cluster, mkv.IDBlockGroup, int64(bg.Len()))
	cluster.Write(bg.Bytes())

	var fullBuf bytes.Buffer
	ebml.WriteElementHeader(&fullBuf, ebml.IDEBMLHeader, 0)
	var segBuf bytes.Buffer
	ebml.WriteElementHeader(&segBuf, mkv.IDCluster, int64(cluster.Len()))
	segBuf.Write(cluster.Bytes())
	ebml.WriteElementHeader(&fullBuf, mkv.IDSegment, int64(segBuf.Len()))
	fullBuf.Write(segBuf.Bytes())
	fullInput := fullBuf.Bytes()

	for limit := 1; limit < len(fullInput); limit++ {
		tr := &truncBlockReader{data: fullInput[:limit]}
		br, err := NewBlockReader(tr, 1000000)
		if err != nil {
			continue
		}
		for {
			_, err := br.Next()
			if err != nil {
				break
			}
		}
	}
}

type truncBlockReader struct {
	data []byte
	pos  int
}

func (r *truncBlockReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *truncBlockReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = int(offset)
	case io.SeekCurrent:
		r.pos += int(offset)
	case io.SeekEnd:
		r.pos = len(r.data) + int(offset)
	}
	if r.pos < 0 {
		r.pos = 0
	}
	if r.pos > len(r.data) {
		r.pos = len(r.data)
	}
	return int64(r.pos), nil
}

func TestTruncatedBlockReaderWithLacing(t *testing.T) {
	// Build blocks with various lacing types
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// Xiph lacing block
	xiphBlock := []byte{
		0x81, 0x00, 0x00, 0x02, 0x01,
		0xFF, 45, // xiph size 300
	}
	xiphBlock = append(xiphBlock, make([]byte, 350)...)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(xiphBlock)))
	cluster.Write(xiphBlock)

	// EBML lacing block
	ebmlBlock := []byte{
		0x81, 0x00, 0x00, 0x06, 0x02,
		0x80 | 100, // first size
		0xC0,       // diff=0
	}
	ebmlBlock = append(ebmlBlock, make([]byte, 250)...)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(ebmlBlock)))
	cluster.Write(ebmlBlock)

	// Fixed lacing block
	fixedBlock := []byte{0x81, 0x00, 0x00, 0x04, 0x01}
	fixedBlock = append(fixedBlock, make([]byte, 20)...)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(fixedBlock)))
	cluster.Write(fixedBlock)

	var fullBuf bytes.Buffer
	ebml.WriteElementHeader(&fullBuf, ebml.IDEBMLHeader, 0)
	var segBuf bytes.Buffer
	ebml.WriteElementHeader(&segBuf, mkv.IDCluster, int64(cluster.Len()))
	segBuf.Write(cluster.Bytes())
	ebml.WriteElementHeader(&fullBuf, mkv.IDSegment, int64(segBuf.Len()))
	fullBuf.Write(segBuf.Bytes())
	fullInput := fullBuf.Bytes()

	for limit := 1; limit < len(fullInput); limit++ {
		tr := &truncBlockReader{data: fullInput[:limit]}
		br, err := NewBlockReader(tr, 1000000)
		if err != nil {
			continue
		}
		for {
			_, err := br.Next()
			if err != nil {
				break
			}
		}
	}
}

func TestBlockReaderTruncWithErr(t *testing.T) {
	// Build a complex block structure
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// SimpleBlock
	sb := []byte{0x81, 0x00, 0x00, 0x80, 0xDE, 0xAD, 0xBE, 0xEF}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(sb)))
	cluster.Write(sb)

	// BlockGroup
	var bg bytes.Buffer
	innerBlock := []byte{0x81, 0x00, 0x00, 0x00, 0xCA, 0xFE}
	ebml.WriteElementHeader(&bg, mkv.IDBlock, int64(len(innerBlock)))
	bg.Write(innerBlock)
	ebml.WriteElementHeader(&cluster, mkv.IDBlockGroup, int64(bg.Len()))
	cluster.Write(bg.Bytes())

	// Xiph lacing
	xiphBlock := []byte{0x81, 0x00, 0x00, 0x02, 0x01, 50}
	xiphBlock = append(xiphBlock, make([]byte, 100)...)
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(xiphBlock)))
	cluster.Write(xiphBlock)

	var fullBuf bytes.Buffer
	ebml.WriteElementHeader(&fullBuf, ebml.IDEBMLHeader, 0)
	var segBuf bytes.Buffer
	ebml.WriteElementHeader(&segBuf, mkv.IDCluster, int64(cluster.Len()))
	segBuf.Write(cluster.Bytes())
	ebml.WriteElementHeader(&fullBuf, mkv.IDSegment, int64(segBuf.Len()))
	fullBuf.Write(segBuf.Bytes())
	fullInput := fullBuf.Bytes()

	// Use errAtReader which returns a real error, not just EOF
	for limit := 1; limit < len(fullInput); limit++ {
		tr := &errAtReader{data: fullInput[:limit]}
		br, err := NewBlockReader(tr, 1000000)
		if err != nil {
			continue
		}
		for {
			_, err := br.Next()
			if err != nil {
				break
			}
		}
	}
}

type errAtReader struct {
	data []byte
	pos  int
}

func (r *errAtReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("errAtReader: limit reached")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	if r.pos >= len(r.data) && n < len(p) {
		return n, fmt.Errorf("errAtReader: partial read")
	}
	return n, nil
}

func (r *errAtReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = int(offset)
	case io.SeekCurrent:
		r.pos += int(offset)
	case io.SeekEnd:
		r.pos = len(r.data) + int(offset)
	}
	if r.pos < 0 {
		r.pos = 0
	}
	if r.pos > len(r.data) {
		r.pos = len(r.data)
	}
	return int64(r.pos), nil
}

func TestBlockWithSegEndReached(t *testing.T) {
	// Build input where segment ends before cluster ends
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	blockPayload := []byte{0x81, 0x00, 0x00, 0x80, 0xDE, 0xAD}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !b.Keyframe {
		t.Error("expected keyframe")
	}
	// Second read should hit EOF
	_, err = br.Next()
	if err != io.EOF {
		t.Errorf("expected EOF, got: %v", err)
	}
}

func TestUnknownSizeElementOutsideCluster(t *testing.T) {
	// Build an MKV where a non-cluster element has unknown size (-1)
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	// Segment with unknown size
	ebml.WriteElementID(&buf, mkv.IDSegment)
	ebml.WriteDataSize(&buf, -1)
	// write a non-cluster element with unknown size inside segment
	ebml.WriteElementID(&buf, mkv.IDInfo)
	ebml.WriteDataSize(&buf, -1)

	r := bytes.NewReader(buf.Bytes())
	br, err := NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	_, err = br.Next()
	if err == nil || !strings.Contains(err.Error(), "unknown-size") {
		t.Fatalf("expected 'unknown-size' error, got: %v", err)
	}
}
