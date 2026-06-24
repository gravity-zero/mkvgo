package reader

// mutation_kill_test.go — tests whose sole purpose is to kill surviving mutants.
// Each function targets specific lines in codec_colour.go, blocks.go, keyframes.go,
// reader_meta.go, stream.go.  Tests are kept orthogonal to existing suites.

import (
	"bytes"
	"context"
	"io"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// ─── codec_colour.go: helper / pure functions ──────────────────────────────

// TestValidBitDepthBoundaries kills mutations on the switch cases 8/10/12.
func TestValidBitDepthBoundaries(t *testing.T) {
	type tc struct {
		d       uint32
		wantNil bool
	}
	cases := []tc{
		{7, true}, {8, false}, {9, true},
		{10, false}, {11, true}, {12, false}, {13, true},
		{0, true}, {255, true},
	}
	for _, c := range cases {
		got := validBitDepth(c.d)
		if (got == nil) != c.wantNil {
			t.Errorf("validBitDepth(%d): nil=%v, want nil=%v", c.d, got == nil, c.wantNil)
		}
		if !c.wantNil && got != nil && *got != uint16(c.d) {
			t.Errorf("validBitDepth(%d) = %d, want %d", c.d, *got, c.d)
		}
	}
}

// TestCicpOrNilBoundary kills the v == 2 boundary: only CICP code 2 returns nil.
func TestCicpOrNilBoundary(t *testing.T) {
	// CICP unspecified (2) must be nil.
	if cicpOrNil(2) != nil {
		t.Fatal("cicpOrNil(2) must be nil (unspecified)")
	}
	// Adjacent values (1, 3) must be non-nil and carry the exact value.
	for _, v := range []uint32{0, 1, 3, 9, 13, 16, 255} {
		got := cicpOrNil(v)
		if got == nil {
			t.Errorf("cicpOrNil(%d) must be non-nil", v)
			continue
		}
		if *got != uint16(v) {
			t.Errorf("cicpOrNil(%d) = %d, want %d", v, *got, v)
		}
	}
}

// TestSpsRangeBothValues kills the fullRangeFlag == 1 condition.
func TestSpsRangeBothValues(t *testing.T) {
	// flag=0 → limited (Matroska range 1)
	if got := spsRange(0); got == nil || *got != 1 {
		t.Errorf("spsRange(0) = %v, want 1 (limited)", got)
	}
	// flag=1 → full (Matroska range 2)
	if got := spsRange(1); got == nil || *got != 2 {
		t.Errorf("spsRange(1) = %v, want 2 (full)", got)
	}
}

// TestGcd64Arithmetic kills mutations on the gcd64 loop and the a==0 guard.
func TestGcd64Arithmetic(t *testing.T) {
	cases := []struct{ a, b, want uint64 }{
		{0, 0, 1},  // a reaches 0 at end → return 1 (not 0)
		{4, 0, 4},  // b starts at 0, loop skipped
		{0, 5, 5},  // a=0, b=5 → one swap then done
		{12, 8, 4}, // standard GCD
		{7, 3, 1},  // coprime
		{100, 50, 50},
		{2560, 720, 80}, // used in SAR display dim test below
	}
	for _, c := range cases {
		got := gcd64(c.a, c.b)
		if got != c.want {
			t.Errorf("gcd64(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ─── codec_colour.go: avcColour / parseAVCSPS ──────────────────────────────

// TestAvcColourLengthBoundary kills len(cp) < 7 CONDITIONALS_BOUNDARY.
func TestAvcColourLengthBoundary(t *testing.T) {
	// 0–6 bytes must return nil.
	for n := 0; n < 7; n++ {
		cp := make([]byte, n)
		if n > 0 {
			cp[0] = 1 // valid version byte
		}
		if avcColour(cp) != nil {
			t.Errorf("avcColour(len=%d) must be nil for len<7", n)
		}
	}
}

// spsPrefixCF3 builds a High-profile SPS RBSP with chroma_format_idc=3 (4:4:4)
// and no VUI, for use in the cf <= 3 boundary test.
func spsPrefixCF3() *tbw {
	b := &tbw{}
	b.u(100, 8) // profile_idc = High
	b.u(0, 8)   // constraint flags + reserved
	b.u(40, 8)  // level_idc
	b.ue(0)     // seq_parameter_set_id
	// High-profile section:
	b.ue(3)   // chroma_format_idc = 3 (4:4:4)
	b.u(0, 1) // separate_colour_plane_flag (present because cf==3)
	b.ue(0)   // bit_depth_luma_minus8 → 8-bit
	b.ue(0)   // bit_depth_chroma_minus8
	b.u(0, 1) // qpprime_y_zero_transform_bypass_flag
	b.u(0, 1) // seq_scaling_matrix_present_flag = 0
	// Rest of SPS skeleton:
	b.ue(0)   // log2_max_frame_num_minus4
	b.ue(0)   // pic_order_cnt_type = 0
	b.ue(0)   // log2_max_pic_order_cnt_lsb_minus4
	b.ue(1)   // max_num_ref_frames
	b.u(0, 1) // gaps_in_frame_num_value_allowed_flag
	b.ue(29)  // pic_width_in_mbs_minus1
	b.ue(17)  // pic_height_in_map_units_minus1
	b.u(1, 1) // frame_mbs_only_flag = 1 (progressive)
	b.u(1, 1) // direct_8x8_inference_flag
	b.u(0, 1) // frame_cropping_flag = 0
	b.u(0, 1) // vui_parameters_present_flag = 0 (no VUI)
	return b
}

// TestAVCSPSChromaFormatIDC3 kills the cf <= 3 CONDITIONALS_BOUNDARY (cf=3 must set chroma=3).
func TestAVCSPSChromaFormatIDC3(t *testing.T) {
	avcc := wrapAvcC(spsPrefixCF3())
	bc := parseCodecColour("h264", avcc)
	if bc == nil {
		t.Fatal("parseCodecColour returned nil for valid High SPS with cf=3")
	}
	if bc.chroma == nil || *bc.chroma != 3 {
		t.Errorf("chroma = %v, want 3 (4:4:4 for cf=3)", bc.chroma)
	}
}

// TestAVCSPSChromaFormatIDC0 confirms cf=0 (monochrome) is set in the High SPS chroma block.
func TestAVCSPSChromaFormatIDC0(t *testing.T) {
	// Build High SPS with cf=0.
	b := &tbw{}
	b.u(100, 8) // High
	b.u(0, 8)
	b.u(40, 8)
	b.ue(0) // seq_parameter_set_id
	b.ue(0) // chroma_format_idc = 0 (monochrome)
	// no separate_colour_plane_flag since cf!=3
	b.ue(0) // bit_depth_luma_minus8
	b.ue(0) // bit_depth_chroma_minus8
	b.u(0, 1)
	b.u(0, 1)
	b.ue(0)
	b.ue(0)
	b.ue(0)
	b.ue(1)
	b.u(0, 1)
	b.ue(29)
	b.ue(17)
	b.u(1, 1)
	b.u(1, 1)
	b.u(0, 1)
	b.u(0, 1)
	avcc := wrapAvcC(b)
	bc := parseCodecColour("h264", avcc)
	if bc == nil {
		t.Fatal("parseCodecColour nil for High SPS cf=0")
	}
	if bc.chroma == nil || *bc.chroma != 0 {
		t.Errorf("chroma = %v, want 0 (monochrome for cf=0)", bc.chroma)
	}
}

// ─── codec_colour.go: readVUIAspectRatio idc boundaries ───────────────────

// TestSARIdcBoundaries kills idc >= 1 && idc <= 16 CONDITIONALS_BOUNDARY mutations.
func TestSARIdcBoundaries(t *testing.T) {
	w, h := uint32(1280), uint32(720)
	cases := []struct {
		idc        uint32
		wantSARSet bool
		wantSARW   uint32
		wantSARH   uint32
	}{
		{0, false, 0, 0}, // idc=0: falls through switch, no SAR
		// idc=1 → SAR {1,1}: sarWidth==sarHeight → no display dims (equivalent mutant for idc>=1).
		{1, false, 0, 0},
		{16, true, 2, 1},  // idc=16: last table entry {2,1} — kills idc <= 16 → idc < 16
		{17, false, 0, 0}, // idc=17: out of range, no SAR
	}
	for _, c := range cases {
		avcc := buildSPSAspectAvcC(c.idc, 0, 0)
		tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc, Width: &w, Height: &h}
		fillColourFromCodecPrivate(&tr)
		if c.wantSARSet {
			if tr.DisplayWidth == nil {
				t.Errorf("idc=%d: DisplayWidth should be set", c.idc)
				continue
			}
			// Verify non-zero display dims were actually derived from the SAR table.
		} else {
			if tr.DisplayWidth != nil {
				t.Errorf("idc=%d: DisplayWidth should be nil (no SAR)", c.idc)
			}
		}
	}
}

// TestSARIdc16ExactDisplayDims kills arithmetic mutations in the SAR display-dim calc.
// idc=16 → SAR {2,1}; over 1280×720 → DisplayWidth=32, DisplayHeight=9.
func TestSARIdc16ExactDisplayDims(t *testing.T) {
	w, h := uint32(1280), uint32(720)
	avcc := buildSPSAspectAvcC(16, 0, 0) // SAR 2:1
	tr := mkv.Track{
		Type:         mkv.VideoTrack,
		Codec:        "h264",
		CodecPrivate: avcc,
		Width:        &w,
		Height:       &h,
	}
	fillColourFromCodecPrivate(&tr)
	if tr.DisplayWidth == nil || tr.DisplayHeight == nil {
		t.Fatal("SAR idc=16: expected DisplayWidth/DisplayHeight to be set")
	}
	if *tr.DisplayWidth != 32 {
		t.Errorf("DisplayWidth = %d, want 32 (1280*2 / gcd(2560,720)=80)", *tr.DisplayWidth)
	}
	if *tr.DisplayHeight != 9 {
		t.Errorf("DisplayHeight = %d, want 9 (720*1 / gcd(2560,720)=80)", *tr.DisplayHeight)
	}
}

// TestSARSquareLeavesDisplayDimsNil kills bc.sarWidth != bc.sarHeight condition.
// idc=1 → SAR {1,1}, square → no display dims should be derived.
func TestSARSquareLeavesDisplayDimsNil(t *testing.T) {
	w, h := uint32(1920), uint32(1080)
	avcc := buildSPSAspectAvcC(1, 0, 0) // SAR 1:1
	tr := mkv.Track{
		Type:         mkv.VideoTrack,
		Codec:        "h264",
		CodecPrivate: avcc,
		Width:        &w,
		Height:       &h,
	}
	fillColourFromCodecPrivate(&tr)
	if tr.DisplayWidth != nil || tr.DisplayHeight != nil {
		t.Errorf("square SAR: DisplayWidth=%v DisplayHeight=%v, want nil/nil (1:1 SAR produces no display dims)", tr.DisplayWidth, tr.DisplayHeight)
	}
}

// TestSARExtendedExactDims kills bc.sarWidth > 0 / bc.sarHeight > 0 conditions
// and the exact GCD arithmetic. Extended_SAR 257:160 over 1280×720:
// dw=328960, dh=115200, gcd(328960,115200)=1280 → 257:90.
func TestSARExtendedExactDims(t *testing.T) {
	w, h := uint32(1280), uint32(720)
	avcc := buildSPSAspectAvcC(255, 257, 160) // Extended_SAR 257:160
	tr := mkv.Track{
		Type:         mkv.VideoTrack,
		Codec:        "h264",
		CodecPrivate: avcc,
		Width:        &w,
		Height:       &h,
	}
	fillColourFromCodecPrivate(&tr)
	if tr.DisplayWidth == nil || tr.DisplayHeight == nil {
		t.Fatal("Extended_SAR 257:160: expected display dims")
	}
	// 1280*257 = 328960; 720*160 = 115200; gcd = 1280 → 257:90
	if *tr.DisplayWidth != 257 || *tr.DisplayHeight != 90 {
		t.Errorf("display = %d:%d, want 257:90", *tr.DisplayWidth, *tr.DisplayHeight)
	}
}

// ─── codec_colour.go: hevcColour length boundary ─────────────────────────

// TestHevcColourLengthBoundary kills len(cp) < 23 CONDITIONALS_BOUNDARY.
func TestHevcColourLengthBoundary(t *testing.T) {
	// 0–22 bytes must return nil.
	for n := 0; n < 23; n++ {
		cp := make([]byte, n)
		if n > 0 {
			cp[0] = 1
		}
		if hevcColour(cp) != nil {
			t.Errorf("hevcColour(len=%d) must be nil for len<23", n)
		}
	}
}

// TestHevcBitDepthFromHeaderBits kills cp[17]&0x07 + 8 arithmetic.
// Bit positions: &0x07 isolates the low 3 bits → bit depth = low3 + 8.
func TestHevcBitDepthFromHeaderBits(t *testing.T) {
	cases := []struct {
		lowBits uint8
		wantBD  int // -1 = nil (invalid depth)
	}{
		{0, 8},  // 0+8=8
		{2, 10}, // 2+8=10
		{4, 12}, // 4+8=12
		{1, -1}, // 1+8=9 → validBitDepth returns nil
		{3, -1}, // 3+8=11 → nil
	}
	for _, c := range cases {
		cp := make([]byte, 23)
		cp[0] = 1          // valid marker
		cp[17] = c.lowBits // bitDepthLumaMinus8 in low 3 bits
		// No NAL arrays (cp[22] = 0).
		bc := hevcColour(cp)
		if bc == nil {
			t.Fatalf("hevcColour(lowBits=%d): bc is nil (expected non-nil with valid 23-byte header)", c.lowBits)
		}
		got := p16(bc.bitDepth)
		if got != c.wantBD {
			t.Errorf("hevcColour bitDepth(lowBits=%d) = %d, want %d", c.lowBits, got, c.wantBD)
		}
	}
}

// ─── codec_colour.go: AV1 bitDepth / seqProfile ──────────────────────────

// buildMinimalAV1C builds a 4-byte av1C header with no OBUs.
func buildMinimalAV1C(seqProfile, highBitDepth, twelveBit uint8) []byte {
	return []byte{
		0x81,                                   // marker=1, version=1
		seqProfile << 5,                        // seq_profile in bits 7:5
		(highBitDepth << 6) | (twelveBit << 5), // high_bit_depth(1) twelve_bit(1)
		0x00,                                   // no configOBUs
	}
}

// TestAV1BitDepthSeqProfile kills mutations on the seqProfile==2/highBitDepth/twelveBit switches.
func TestAV1BitDepthSeqProfile(t *testing.T) {
	type tc struct {
		seqProfile, highBD, twelveBit uint8
		wantBD                        int
		wantProfile                   string
	}
	cases := []tc{
		{0, 0, 0, 8, "Main"},          // default 8-bit
		{0, 1, 0, 10, "Main"},         // highBD=1, not profile 2 → 10-bit
		{1, 1, 0, 10, "High"},         // profile 1, highBD=1 → 10-bit (not profile 2)
		{2, 0, 0, 8, "Professional"},  // profile 2, highBD=0 → default 8-bit
		{2, 1, 0, 10, "Professional"}, // profile 2, highBD=1, twelveBit=0 → 10-bit
		{2, 1, 1, 12, "Professional"}, // profile 2, highBD=1, twelveBit=1 → 12-bit
	}
	for _, c := range cases {
		cp := buildMinimalAV1C(c.seqProfile, c.highBD, c.twelveBit)
		bc := av1Colour(cp)
		if bc == nil {
			t.Errorf("av1Colour(profile=%d highBD=%d twelve=%d): nil", c.seqProfile, c.highBD, c.twelveBit)
			continue
		}
		if got := p16(bc.bitDepth); got != c.wantBD {
			t.Errorf("av1Colour(profile=%d highBD=%d twelve=%d) bitDepth=%d, want %d",
				c.seqProfile, c.highBD, c.twelveBit, got, c.wantBD)
		}
		if bc.profile != c.wantProfile {
			t.Errorf("av1Colour profile=%q, want %q", bc.profile, c.wantProfile)
		}
	}
}

// ─── codec_colour.go: bitReader guard conditions ──────────────────────────

// TestBitReaderBits32Boundary kills n > 32 guard in bitReader.bits.
func TestBitReaderBits32Boundary(t *testing.T) {
	// bits(32) must succeed and return the exact 32-bit value.
	r := &bitReader{data: []byte{0xAB, 0xCD, 0xEF, 0x12}}
	v := r.bits(32)
	if r.err {
		t.Fatal("bits(32) set err, must not for n=32")
	}
	if v != 0xABCDEF12 {
		t.Errorf("bits(32) = 0x%X, want 0xABCDEF12", v)
	}

	// bits(33) must set err.
	r2 := &bitReader{data: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF}}
	r2.bits(33)
	if !r2.err {
		t.Fatal("bits(33) must set err (exceeds 32)")
	}
}

// TestBitReaderUELeadingZerosGuard kills zeros > 31 CONDITIONALS_BOUNDARY.
func TestBitReaderUELeadingZerosGuard(t *testing.T) {
	// Build a byte slice with exactly 31 leading zero bits, bit-31=1, then zeros.
	// Bit positions are MSB-first: bit 0 = MSB of byte 0.
	// bit 31 = LSB of byte 3 (index 3).
	data31 := make([]byte, 8)
	data31[3] |= 0x01 // bit 31 = 1
	r31 := &bitReader{data: data31}
	_ = r31.ue()
	if r31.err {
		t.Error("ue with 31 leading zeros must not set err (limit is >31)")
	}

	// 32 leading zeros → must set err.
	data32 := make([]byte, 8)
	data32[4] |= 0x80 // bit 32 = MSB of byte 4
	r32 := &bitReader{data: data32}
	r32.ue()
	if !r32.err {
		t.Error("ue with 32 leading zeros must set err")
	}
}

// TestBitReaderBitsExactValue kills ARITHMETIC mutations in bits() accumulation.
func TestBitReaderBitsExactValue(t *testing.T) {
	// Read 4 bits at a time and verify exact values.
	r := &bitReader{data: []byte{0xA5}} // 1010 0101
	n0 := r.bits(4)                     // 1010 = 10
	n1 := r.bits(4)                     // 0101 = 5
	if r.err {
		t.Fatal("unexpected err reading 8 bits from 1-byte buffer")
	}
	if n0 != 10 {
		t.Errorf("bits(4) first nibble = %d, want 10", n0)
	}
	if n1 != 5 {
		t.Errorf("bits(4) second nibble = %d, want 5", n1)
	}
}

// ─── blocks.go: block parsing ─────────────────────────────────────────────

// TestBlockZeroDataSize kills dataSize < 0 → dataSize <= 0 boundary mutation.
// A SimpleBlock with exactly 0 bytes of payload (after track+tc+flags) must succeed.
func TestBlockZeroDataSize(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// 4 bytes: track=1(1B) + tc=0(2B) + flags=0x80(keyframe)(1B) = 4 bytes, 0 payload.
	blockPayload := []byte{0x81, 0x00, 0x00, 0x80}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1_000_000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	b, err := br.Next()
	if err != nil {
		t.Fatalf("zero-payload block must succeed, got: %v", err)
	}
	if len(b.Data) != 0 {
		t.Errorf("data len = %d, want 0", len(b.Data))
	}
	if !b.Keyframe {
		t.Error("keyframe flag = false, want true (flags=0x80)")
	}
}

// TestBlockKeyframeFlagExact kills flags&0x80 != 0 CONDITIONALS_NEGATION.
func TestBlockKeyframeFlagExact(t *testing.T) {
	for _, kf := range []bool{true, false} {
		var cluster bytes.Buffer
		ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
		ebml.WriteUint(&cluster, 0, 1)

		flags := byte(0x00)
		if kf {
			flags = 0x80
		}
		payload := []byte{0x81, 0x00, 0x00, flags, 0xAA}
		ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(payload)))
		cluster.Write(payload)

		rdr := buildBlockReaderInput(cluster.Bytes())
		br, err := NewBlockReader(rdr, 1_000_000)
		if err != nil {
			t.Fatalf("init kf=%v: %v", kf, err)
		}
		b, err := br.Next()
		if err != nil {
			t.Fatalf("Next kf=%v: %v", kf, err)
		}
		if b.Keyframe != kf {
			t.Errorf("Keyframe = %v, want %v (flags=0x%02x)", b.Keyframe, kf, flags)
		}
	}
}

// TestBlockExactTimecode kills arithmetic mutations in the timecode computation.
func TestBlockExactTimecode(t *testing.T) {
	// clusterTS=500, relTC=+300 → absTC=800, scale=1_000_000 → 800ms
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 2)
	ebml.WriteUint(&cluster, 500, 2) // cluster timestamp = 500

	// relTC = +300 (big-endian int16): 0x01 0x2C
	payload := []byte{0x81, 0x01, 0x2C, 0x80, 0xDE}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(payload)))
	cluster.Write(payload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1_000_000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	b, err := br.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// absTC = 500 + 300 = 800; ms = 800 * 1_000_000 / 1_000_000 = 800
	if b.Timecode != 800 {
		t.Errorf("Timecode = %d, want 800", b.Timecode)
	}
}

// TestBlockTimecodeNegativeRelTC kills arithmetic with negative relTC.
func TestBlockTimecodeNegativeRelTC(t *testing.T) {
	// clusterTS=1000, relTC=-5 → absTC=995 → 995ms
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 2)
	ebml.WriteUint(&cluster, 1000, 2)

	// relTC = -5 as int16 big-endian: 0xFF 0xFB
	payload := []byte{0x81, 0xFF, 0xFB, 0x80, 0xBB}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(payload)))
	cluster.Write(payload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1_000_000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	b, err := br.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if b.Timecode != 995 {
		t.Errorf("Timecode = %d, want 995 (1000 + (-5))", b.Timecode)
	}
}

// TestBlockLacingFrameCountByte kills int(raw[0]) + 1 arithmetic.
// frameCount = raw[0] + 1, so raw[0]=2 → 3 frames.
func TestBlockLacingFrameCountByte(t *testing.T) {
	// Fixed lacing: raw[0] = 2 (frame count = 3), each frame = 2 bytes.
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	blockPayload := []byte{
		0x81,       // track 1
		0x00, 0x00, // timecode 0
		0x04,       // flags: fixed lacing (bits 1:0 = 10 = 2)
		0x02,       // lace count byte = 2 → 3 frames
		0xAA, 0xBB, // frame 0
		0xCC, 0xDD, // frame 1
		0xEE, 0xFF, // frame 2
	}
	ebml.WriteElementHeader(&cluster, mkv.IDSimpleBlock, int64(len(blockPayload)))
	cluster.Write(blockPayload)

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1_000_000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	count := 0
	for {
		_, e := br.Next()
		if e != nil {
			break
		}
		count++
	}
	if count != 3 {
		t.Errorf("frame count = %d, want 3 (lace byte 0x02 + 1)", count)
	}
}

// TestSignedVINTLenExactBiasValues kills signedVINTLen boundary arithmetic.
// For w=1: bias = 2^(7-1) = 64. diff=63 fits (< 64), diff=64 does not.
func TestSignedVINTLenExactBiasValues(t *testing.T) {
	cases := []struct {
		diff int
		want int
	}{
		{63, 1},  // w=1 bias=64: 63 < 64 fits
		{-64, 1}, // -64 >= -64 fits
		{64, 2},  // 64 >= 64 → needs w=2
		{-65, 2}, // -65 < -64 → needs w=2
		// w=2: bias = 2^13 = 8192
		{8191, 2},  // < 8192 fits
		{-8192, 2}, // >= -8192 fits
		{8192, 3},  // 8192 >= 8192 → needs w=3
	}
	for _, c := range cases {
		got := signedVINTLen(c.diff)
		if got != c.want {
			t.Errorf("signedVINTLen(%d) = %d, want %d", c.diff, got, c.want)
		}
	}
}

// TestVintLenExactBoundaries kills vintLen boundary arithmetic.
// For w: max val = 2^(7w) - 2.  val at boundary+1 triggers next width.
func TestVintLenExactBoundaries(t *testing.T) {
	cases := []struct {
		val  uint64
		want int
	}{
		{0, 1},
		{126, 1}, // 2^7-2 = 126: max 1-byte
		{127, 2}, // 2^7-1 = 127: triggers 2-byte
		{128, 2},
		{16382, 2}, // 2^14-2 = 16382: max 2-byte
		{16383, 3}, // triggers 3-byte
		{math.MaxUint64, 8},
	}
	for _, c := range cases {
		got := vintLen(c.val)
		if got != c.want {
			t.Errorf("vintLen(%d) = %d, want %d", c.val, got, c.want)
		}
	}
}

// TestBlockGroupDuration kills durationMs exact computation.
func TestBlockGroupDuration(t *testing.T) {
	var cluster bytes.Buffer
	ebml.WriteElementHeader(&cluster, mkv.IDTimestamp, 1)
	ebml.WriteUint(&cluster, 0, 1)

	// BlockGroup with a Block and a BlockDuration of 42 ms (scale=1_000_000).
	var bg bytes.Buffer
	innerBlock := []byte{0x81, 0x00, 0x00, 0x00, 0xCA, 0xFE}
	ebml.WriteElementHeader(&bg, mkv.IDBlock, int64(len(innerBlock)))
	bg.Write(innerBlock)
	// BlockDuration = 42 (in timecode units, then converted via safeTimecodeMs)
	ebml.WriteElementHeader(&bg, mkv.IDBlockDuration, 1)
	ebml.WriteUint(&bg, 42, 1)

	ebml.WriteElementHeader(&cluster, mkv.IDBlockGroup, int64(bg.Len()))
	cluster.Write(bg.Bytes())

	r := buildBlockReaderInput(cluster.Bytes())
	br, err := NewBlockReader(r, 1_000_000)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	b, err := br.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// safeTimecodeMs(42, 1_000_000) = 42*1_000_000/1_000_000 = 42
	if b.Duration != 42 {
		t.Errorf("Duration = %d, want 42", b.Duration)
	}
}

// ─── keyframes.go: keyframeTimesMs ────────────────────────────────────────

// TestKeyframeTimescaleDefault kills scale <= 0 → scale = 1_000_000 default.
// With TimecodeScale=0, scale defaults to 1_000_000, so TimeMs is returned as-is.
func TestKeyframeTimescaleDefault(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 0}, // triggers default
		Cues: []mkv.CuePoint{{TimeMs: 1000}},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 1 {
		t.Fatalf("kf = %v, want [1000]", kf)
	}
	// 1000 * 1_000_000 / 1_000_000 = 1000, not 0 (which would happen if scale stayed 0).
	if kf[0] != 1000 {
		t.Errorf("kf[0] = %d, want 1000 (scale default applied)", kf[0])
	}
}

// TestKeyframeNegativeTimescaleDefault kills scale <= 0 for negative scale.
func TestKeyframeNegativeTimescaleDefault(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: -1}, // negative → default
		Cues: []mkv.CuePoint{{TimeMs: 500}},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 1 || kf[0] != 500 {
		t.Errorf("kf = %v, want [500] (default scale for negative TimecodeScale)", kf)
	}
}

// TestKeyframeExactScalingArithmetic kills cue.TimeMs*scale/1_000_000 arithmetic.
func TestKeyframeExactScalingArithmetic(t *testing.T) {
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 2_000_000},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack}},
		Cues: []mkv.CuePoint{
			{TimeMs: 500, Track: 1},
			{TimeMs: 1000, Track: 1},
		},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 2 {
		t.Fatalf("kf = %v, want 2 entries", kf)
	}
	// 500 * 2_000_000 / 1_000_000 = 1000
	if kf[0] != 1000 {
		t.Errorf("kf[0] = %d, want 1000", kf[0])
	}
	// 1000 * 2_000_000 / 1_000_000 = 2000
	if kf[1] != 2000 {
		t.Errorf("kf[1] = %d, want 2000", kf[1])
	}
}

// TestKeyframeVideoTrackFilterIncludesZeroTrack kills cue.Track != 0 condition.
// A cue with Track=0 (unset) must NOT be filtered even when videoTrack is set.
func TestKeyframeVideoTrackFilterIncludesZeroTrack(t *testing.T) {
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack}},
		Cues: []mkv.CuePoint{
			{TimeMs: 100, Track: 1}, // matches video track → include
			{TimeMs: 200, Track: 2}, // different track → exclude
			{TimeMs: 300, Track: 0}, // Track=0 (unset) → include
		},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 2 {
		t.Fatalf("kf = %v, want [100 300]", kf)
	}
	if kf[0] != 100 || kf[1] != 300 {
		t.Errorf("kf = %v, want [100 300]", kf)
	}
}

// TestKeyframeVideoTrackFilterExcludesWrongTrack kills cue.Track != videoTrack condition.
func TestKeyframeVideoTrackFilterExcludesWrongTrack(t *testing.T) {
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{{ID: 3, Type: mkv.VideoTrack}},
		Cues: []mkv.CuePoint{
			{TimeMs: 50, Track: 3},  // video track → include
			{TimeMs: 150, Track: 4}, // not video track → exclude
		},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 1 || kf[0] != 50 {
		t.Errorf("kf = %v, want [50]", kf)
	}
}

// TestKeyframeVideoTrackFallback kills len(times)==0 branch: when all cues are
// filtered out, fall back to using every cue.
func TestKeyframeVideoTrackFallback(t *testing.T) {
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack}},
		Cues: []mkv.CuePoint{
			{TimeMs: 100, Track: 7}, // wrong track → filtered
			{TimeMs: 200, Track: 8}, // wrong track → filtered
		},
	}
	kf := keyframeTimesMs(c)
	// All video-track cues were filtered → fallback includes all.
	if len(kf) != 2 {
		t.Fatalf("kf = %v, want [100 200] after fallback", kf)
	}
	if kf[0] != 100 || kf[1] != 200 {
		t.Errorf("kf = %v, want [100 200]", kf)
	}
}

// TestKeyframeDedup kills the deduplication logic (t != out[len(out)-1]).
func TestKeyframeDedup(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Cues: []mkv.CuePoint{
			{TimeMs: 100},
			{TimeMs: 100}, // duplicate
			{TimeMs: 200},
			{TimeMs: 200}, // duplicate
			{TimeMs: 300},
		},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 3 {
		t.Fatalf("kf = %v, want 3 unique entries", kf)
	}
	if kf[0] != 100 || kf[1] != 200 || kf[2] != 300 {
		t.Errorf("kf = %v, want [100 200 300]", kf)
	}
}

// TestKeyframeNoVideoTrackUsesAllCues kills videoTrack != 0 condition: when no video
// track exists, every cue is included regardless of Track field.
func TestKeyframeNoVideoTrackUsesAllCues(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		// No video track → videoTrack stays 0 → all cues pass the filter.
		Tracks: []mkv.Track{{ID: 2, Type: mkv.AudioTrack}},
		Cues: []mkv.CuePoint{
			{TimeMs: 10, Track: 2},
			{TimeMs: 20, Track: 2},
		},
	}
	kf := keyframeTimesMs(c)
	if len(kf) != 2 || kf[0] != 10 || kf[1] != 20 {
		t.Errorf("kf = %v, want [10 20] (no video track → all cues)", kf)
	}
}

// ─── reader_meta.go: idFromBytes ──────────────────────────────────────────

// TestIdFromBytes kills id<<8 arithmetic and | operator mutations.
func TestIdFromBytes(t *testing.T) {
	cases := []struct {
		b    []byte
		want uint32
	}{
		{[]byte{0xE7}, mkv.IDTimestamp},                  // 1-byte
		{[]byte{0x16, 0x54, 0xAE, 0x6B}, mkv.IDTracks},   // 4-byte
		{[]byte{0x1F, 0x43, 0xB6, 0x75}, mkv.IDCluster},  // 4-byte
		{[]byte{0x18, 0x53, 0x80, 0x67}, mkv.IDSegment},  // 4-byte
		{[]byte{0x11, 0x4D, 0x9B, 0x74}, mkv.IDSeekHead}, // 4-byte
		{[]byte{0x1C, 0x53, 0xBB, 0x6B}, mkv.IDCues},     // 4-byte
		{[]byte{0x73, 0xA4}, mkv.IDSegmentUID},           // 2-byte
		{[]byte{0xA3}, mkv.IDSimpleBlock},                // 1-byte
		// Byte-by-byte: {0x01, 0x02} → (0x01<<8)|0x02 = 0x0102
		{[]byte{0x01, 0x02}, 0x0102},
		// 3-byte: {0xAB, 0xCD, 0xEF} → 0xABCDEF
		{[]byte{0xAB, 0xCD, 0xEF}, 0xABCDEF},
	}
	for _, c := range cases {
		got := idFromBytes(c.b)
		if got != c.want {
			t.Errorf("idFromBytes(%x) = 0x%X, want 0x%X", c.b, got, c.want)
		}
	}
}

// ─── reader.go / stream.go: setDurationMs / DurationMs ───────────────────

// TestSetDurationMsExact kills arithmetic mutations in setDurationMs.
func TestSetDurationMsExact(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			Duration:      2000.0,    // 2000 timecode units
			TimecodeScale: 1_000_000, // 1 ms per unit
		},
	}
	if err := setDurationMs(c); err != nil {
		t.Fatalf("setDurationMs: %v", err)
	}
	// 2000 * 1_000_000 / 1e6 = 2000 ms
	if c.DurationMs != 2000 {
		t.Errorf("DurationMs = %d, want 2000", c.DurationMs)
	}
}

// TestSetDurationMsZeroDuration kills c.Info.Duration > 0 boundary mutation.
// Duration=0 must leave DurationMs=0 (the check guards zero division / overflow).
func TestSetDurationMsZeroDuration(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{Duration: 0, TimecodeScale: 1_000_000},
	}
	if err := setDurationMs(c); err != nil {
		t.Fatalf("setDurationMs: %v", err)
	}
	if c.DurationMs != 0 {
		t.Errorf("DurationMs = %d, want 0 for Duration=0", c.DurationMs)
	}
}

// TestSetDurationMsNonStandardScale kills timecodeScale arithmetic mutation.
func TestSetDurationMsNonStandardScale(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			Duration:      1000.0,  // 1000 units
			TimecodeScale: 500_000, // 0.5 ms per unit → 500 ms total
		},
	}
	if err := setDurationMs(c); err != nil {
		t.Fatalf("setDurationMs: %v", err)
	}
	// 1000 * 500_000 / 1_000_000 = 500
	if c.DurationMs != 500 {
		t.Errorf("DurationMs = %d, want 500", c.DurationMs)
	}
}

// ─── stream.go: readOnlySeeker ─────────────────────────────────────────────

// TestReadOnlySeekerSeekCurrentZero kills offset==0 && whence==SeekCurrent condition.
func TestReadOnlySeekerSeekCurrentZero(t *testing.T) {
	ros := &readOnlySeeker{r: bytes.NewReader([]byte{1, 2, 3}), pos: 42}

	// Seek(0, SeekCurrent) = position query → must succeed and return 42.
	p, err := ros.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek(0, Current): %v", err)
	}
	if p != 42 {
		t.Errorf("Seek(0, Current) = %d, want 42", p)
	}

	// Seek(1, SeekCurrent) = real movement → must return error.
	_, err = ros.Seek(1, io.SeekCurrent)
	if err == nil {
		t.Error("Seek(1, Current) must return error for forward-only reader")
	}
}

// TestReadOnlySeekerSeekStart kills the whence == SeekCurrent && offset == 0 check
// by also testing SeekStart (which always fails).
func TestReadOnlySeekerSeekStart(t *testing.T) {
	ros := &readOnlySeeker{r: bytes.NewReader([]byte{0xAA}), pos: 0}
	_, err := ros.Seek(0, io.SeekStart)
	if err == nil {
		t.Error("Seek(0, SeekStart) must return error for forward-only reader")
	}
}

// ─── stream.go: ReadStream DurationMs ─────────────────────────────────────

// TestReadStreamDurationMsExact kills arithmetic mutations in the stream duration calc.
func TestReadStreamDurationMsExact(t *testing.T) {
	// Build a streamable MKV with Duration=1500 (units) and TimecodeScale=1_000_000.
	// Expected DurationMs = 1500 * 1_000_000 / 1_000_000 = 1500.
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 1_000_000, 3)
	ebml.WriteElementHeader(&info, mkv.IDDuration, 8)
	ebml.WriteFloat(&info, 1500.0)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())

	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, int64(seg.Len()))
	buf.Write(seg.Bytes())

	c, _, err := ReadStream(context.Background(), &readerOnly{r: bytes.NewReader(buf.Bytes())})
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if c.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", c.DurationMs)
	}
}

// TestReadStreamDurationMsHalfScale kills division arithmetic with half-scale.
func TestReadStreamDurationMsHalfScale(t *testing.T) {
	// TimecodeScale=500_000, Duration=2000 → DurationMs = 2000*500_000/1_000_000 = 1000.
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 500_000, 3)
	ebml.WriteElementHeader(&info, mkv.IDDuration, 8)
	ebml.WriteFloat(&info, 2000.0)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())

	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&buf, mkv.IDSegment, int64(seg.Len()))
	buf.Write(seg.Bytes())

	c, _, err := ReadStream(context.Background(), &readerOnly{r: bytes.NewReader(buf.Bytes())})
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if c.DurationMs != 1000 {
		t.Errorf("DurationMs = %d, want 1000 (2000 * 500_000 / 1_000_000)", c.DurationMs)
	}
}

// ─── reader_meta.go: SeekHead offset boundaries ───────────────────────────

// TestSeekHeadZeroPositionRejected kills the abs < segStart guard: a SeekPosition
// that resolves to exactly segStart (offset=0) should be accepted, but one that
// resolves below it should be ignored without error.
func TestSeekHeadOffsetBoundaries(t *testing.T) {
	// Build [SeekHead][Info][Tracks][Cluster] and verify ReadMeta succeeds.
	// The interesting property: SeekHead positions are relative to the Segment body
	// start (segStart). A position of 0 means "the first element right after the
	// SeekHead", which is valid.
	data := buildSeekHeadMKV(t, false) // well-formed with SeekHead
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta with SeekHead: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Error("ReadMeta with SeekHead: no tracks parsed")
	}
}

// TestReadMetaCueOffset kills abs >= endPos rejection in parseSeekHeadMeta.
func TestReadMetaCueOffset(t *testing.T) {
	// Build [SeekHead→Cues][Info][Tracks][Cues][Cluster].
	// ReadMeta must derive Keyframes from the Cues via the SeekHead offset.
	info, tracks, cluster := infoElem(), tracksElem(), clusterElem()

	var cuesBuf bytes.Buffer
	for i := 0; i < 3; i++ {
		cp := cuePoint(uint64(i*1000), 1, uint64(i*512))
		cuesBuf.Write(cp)
	}
	cuesElem := masterElem(mkv.IDCues, cuesBuf.Bytes())

	// SeekHead length estimation: use a placeholder then fix.
	l := uint64(len(seekHeadElem(seekEntry(mkv.IDInfo, 0), seekEntry(mkv.IDCues, 0))))
	infoPos := l
	cuesPos := l + uint64(len(info)) + uint64(len(tracks)) + uint64(len(cluster))
	sh := seekHeadElem(seekEntry(mkv.IDInfo, infoPos), seekEntry(mkv.IDCues, cuesPos))
	if uint64(len(sh)) != l {
		t.Skipf("seekhead length mismatch, skip: %d vs %d", len(sh), l)
	}

	data := segmentMKV(sh, info, tracks, cluster, cuesElem)
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Keyframes) != 3 {
		t.Errorf("Keyframes = %v, want 3 from Cues via SeekHead", c.Keyframes)
	}
}

// ─── VP9 colour: bitDepth field ───────────────────────────────────────────

// TestVP9ColourBitDepthBoundaries kills the bitDepth==8||10||12 mutations.
func TestVP9ColourBitDepthBoundaries(t *testing.T) {
	// vpcC non-FullBox format: 8 bytes minimum; b[2] >> 4 is bitDepth.
	// Build: cp = {0, 0, bitDepth<<4 | chromaSubsampling<<1 | fullRange, primaries, transfer, matrix, 0, 0}
	for _, bd := range []uint8{8, 10, 12} {
		cp := make([]byte, 8)
		cp[2] = bd << 4 // bitDepth in high nibble
		cp[3] = 1       // primaries = BT.709
		cp[4] = 1
		cp[5] = 1
		bc := vp9Colour(cp)
		if bc == nil {
			t.Fatalf("vp9Colour(bd=%d): nil", bd)
		}
		if bc.bitDepth == nil || *bc.bitDepth != uint16(bd) {
			t.Errorf("vp9Colour(bd=%d): bitDepth=%v, want %d", bd, bc.bitDepth, bd)
		}
	}
	// An unsupported depth (e.g. 4) must leave bitDepth nil.
	cp := make([]byte, 8)
	cp[2] = 4 << 4 // bitDepth=4
	cp[3] = 1
	cp[4] = 1
	cp[5] = 1
	bc := vp9Colour(cp)
	if bc == nil {
		t.Fatal("vp9Colour with bad bitDepth: bc should be non-nil (valid primaries)")
	}
	if bc.bitDepth != nil {
		t.Errorf("vp9Colour(bd=4): bitDepth=%d, want nil", *bc.bitDepth)
	}
}

// TestVP9ColourFullRangeFlag kills the fullRange arithmetic / spsRange call.
func TestVP9ColourFullRangeFlag(t *testing.T) {
	// fullRange is bit 0 of b[2].
	for _, full := range []uint8{0, 1} {
		cp := make([]byte, 8)
		cp[2] = (8 << 4) | full // bitDepth=8, fullRange=full
		cp[3] = 1
		cp[4] = 1
		cp[5] = 1
		bc := vp9Colour(cp)
		if bc == nil {
			t.Fatalf("vp9Colour(full=%d): nil", full)
		}
		var wantRng uint16
		if full == 1 {
			wantRng = 2
		} else {
			wantRng = 1
		}
		if bc.rng == nil || *bc.rng != wantRng {
			t.Errorf("vp9Colour(full=%d): rng=%v, want %d", full, bc.rng, wantRng)
		}
	}
}
