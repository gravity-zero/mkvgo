package reader

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// codec_colour_kill_test.go - direct unit tests of the bitstream-parsing
// primitives (bit reader, Exp-Golomb, nonEmpty) and the colour-fill precedence.
// Asserting exact decoded values pins the arithmetic/boundary mutations gremlins
// left surviving in codec_colour.go, which the higher-level crafted-SPS tests do
// not distinguish.

// TestBitReaderPrimitives asserts exact decoded values, killing the bit/bits/ue/
// se/ueMax arithmetic and boundary mutations.
func TestBitReaderPrimitives(t *testing.T) {
	// bit(): 0x80 = 1000_0000
	r := &bitReader{data: []byte{0x80}}
	if r.bit() != 1 || r.bit() != 0 {
		t.Error("bit() should read MSB-first 1 then 0")
	}
	// bits(): 0xA5 = 1010_0101 → bits(4)=0xA, bits(4)=0x5; bits(8)=0xA5
	if v := (&bitReader{data: []byte{0xA5}}).bits(8); v != 0xA5 {
		t.Errorf("bits(8) = %#x, want 0xA5", v)
	}
	r2 := &bitReader{data: []byte{0xA5}}
	if r2.bits(4) != 0xA || r2.bits(4) != 0x5 {
		t.Error("bits(4)+bits(4) should split 0xA5 into 0xA,0x5")
	}
	// bits(n) with n>32 must flag err and return 0.
	rbad := &bitReader{data: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF}}
	if rbad.bits(33) != 0 || !rbad.err {
		t.Error("bits(33) must set err and return 0")
	}

	// ue(): "1"→0, "010"→1, "00100"→3.
	for _, c := range []struct {
		data []byte
		want uint32
	}{
		{[]byte{0x80}, 0}, // 1
		{[]byte{0x50}, 1}, // 010 1...
		{[]byte{0x20}, 3}, // 00100...
	} {
		if v := (&bitReader{data: c.data}).ue(); v != c.want {
			t.Errorf("ue(%08b) = %d, want %d", c.data[0], v, c.want)
		}
	}

	// se(): k=ue then ±; "1"→0, "010"→+1, "011"→-1, "00100"→+2.
	for _, c := range []struct {
		data []byte
		want int32
	}{
		{[]byte{0x80}, 0},  // ue 0 → 0
		{[]byte{0x50}, 1},  // ue 1 → +1
		{[]byte{0x60}, -1}, // ue 2 → -1
		{[]byte{0x20}, 2},  // ue 3 → +2
	} {
		if v := (&bitReader{data: c.data}).se(); v != c.want {
			t.Errorf("se(%08b) = %d, want %d", c.data[0], v, c.want)
		}
	}

	// ueMax boundary: ue value 3, max 3 → 3 (v>max is false at equality); max 2 → err,0.
	if v := (&bitReader{data: []byte{0x20}}).ueMax(3); v != 3 {
		t.Errorf("ueMax(3) on ue=3 = %d, want 3 (equality allowed)", v)
	}
	rm := &bitReader{data: []byte{0x20}}
	if v := rm.ueMax(2); v != 0 || !rm.err {
		t.Error("ueMax(2) on ue=3 must err and return 0")
	}
}

// TestBitstreamColourNonEmpty kills the per-operand mutations of the nonEmpty()
// `||` chain: each single field must make it true, an empty struct false.
func TestBitstreamColourNonEmpty(t *testing.T) {
	if (&bitstreamColour{}).nonEmpty() {
		t.Error("empty bitstreamColour must be nonEmpty()==false")
	}
	cases := []*bitstreamColour{
		{primaries: u16p(1)}, {transfer: u16p(1)}, {matrix: u16p(1)}, {rng: u16p(1)},
		{bitDepth: u16p(8)}, {profile: "Main"}, {level: u16p(40)},
		{sarWidth: 16}, {chroma: u16p(1)}, {fieldOrder: "progressive"},
	}
	for i, bc := range cases {
		if !bc.nonEmpty() {
			t.Errorf("case %d: a single set field must make nonEmpty()==true", i)
		}
	}
}

// TestAvcColourLengthAndCountBounds kills the boundary checks in avcColour:
// len<7, version!=1, numSPS, and the truncated-SPS guards.
func TestAvcColourLengthAndCountBounds(t *testing.T) {
	// A valid minimal avcC built from a known-good High SPS must parse.
	good := buildHighSPSAvcC(2, 2, 1) // primaries/transfer unspecified, matrix bt709
	if avcColour(good) == nil {
		t.Fatal("valid avcC should parse")
	}
	// Exactly 6 bytes → too short (len < 7).
	if avcColour(good[:6]) != nil {
		t.Error("6-byte avcC must be rejected (len < 7)")
	}
	// version byte != 1 → nil.
	bad := append([]byte(nil), good...)
	bad[0] = 0
	if avcColour(bad) != nil {
		t.Error("avcC with version != 1 must be rejected")
	}
	// numSPS = 0 → no SPS parsed → nil.
	zero := append([]byte(nil), good...)
	zero[5] = 0xE0 // low 5 bits = 0
	if avcColour(zero) != nil {
		t.Error("avcC with numSPS 0 must yield nil")
	}
	// Truncated SPS length (declared longer than the buffer) → nil.
	trunc := good[:len(good)-1]
	if avcColour(trunc) != nil {
		t.Error("avcC with a truncated SPS must be rejected")
	}
}

// TestFillColourFieldOrderPrecedence kills the field_order fill conditionals:
// a container-supplied value wins; an empty one is filled from the bitstream.
func TestFillColourFieldOrderPrecedence(t *testing.T) {
	avcc := buildHighSPSAvcC(2, 2, 1) // frame_mbs_only=1 → bc.fieldOrder "progressive"
	// Container already set FieldOrder → must be kept (kills `== ""` → `!= ""`).
	kept := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc, FieldOrder: "interlaced"}
	fillColourFromCodecPrivate(&kept)
	if kept.FieldOrder != "interlaced" {
		t.Errorf("container FieldOrder must win, got %q", kept.FieldOrder)
	}
	// Empty FieldOrder → filled from the SPS.
	filled := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	fillColourFromCodecPrivate(&filled)
	if filled.FieldOrder != "progressive" {
		t.Errorf("empty FieldOrder should be filled to progressive, got %q", filled.FieldOrder)
	}
}

// TestFillColourSARBoundaries kills the `> 0` boundary checks guarding the SAR→
// display-dimension derivation: a zero SAR or zero coded dimension yields no
// display dimensions.
func TestFillColourSARBoundaries(t *testing.T) {
	w, h := uint32(1920), uint32(1080)
	// Valid Extended_SAR 16:11 with real dimensions → display dims set.
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildSPSAspectAvcC(255, 16, 11),
		Width: &w, Height: &h}
	fillColourFromCodecPrivate(&tr)
	if tr.DisplayWidth == nil {
		t.Error("non-square SAR with real dims should set DisplayWidth")
	}
	// sar_width = 0 → must NOT set display dims (kills bc.sarWidth > 0 → >= 0).
	zeroSAR := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildSPSAspectAvcC(255, 0, 11),
		Width: &w, Height: &h}
	fillColourFromCodecPrivate(&zeroSAR)
	if zeroSAR.DisplayWidth != nil {
		t.Errorf("zero sar_width must not set DisplayWidth, got %v", *zeroSAR.DisplayWidth)
	}
	// Zero coded width → must NOT set display dims (kills *t.Width > 0 → >= 0).
	zw := uint32(0)
	zeroW := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildSPSAspectAvcC(255, 16, 11),
		Width: &zw, Height: &h}
	fillColourFromCodecPrivate(&zeroW)
	if zeroW.DisplayWidth != nil {
		t.Errorf("zero coded width must not set DisplayWidth, got %v", *zeroW.DisplayWidth)
	}
}
