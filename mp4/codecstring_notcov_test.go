package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// This file targets mutation-testing NOT COVERED lines in codecstring.go,
// codec.go and sample.go that no existing test exercised: hevcCodecString and
// av1CodecString (pure functions never reached because existing fixtures are
// H.264/AAC), the vp9 case of rfc6381Codec, vpcCRecord's two accepted forms,
// the VP9 profile-1/3 chroma switch in parseVP9FrameHeader, and the
// reconstructTiming last-sample duration branches.

// ─── codecstring.go: hevcCodecString ────────────────────────────────────────

// codecstrHVCC builds a minimal HEVCDecoderConfigurationRecord long enough for
// hevcCodecString: only the byte offsets it reads (1, 2-5, 6-11, 12) matter.
func codecstrHVCC(profileSpace, tier, profileIDC byte, compat uint32, constraints [6]byte, level byte) []byte {
	b := make([]byte, 13)
	b[1] = profileSpace<<6 | tier<<5 | profileIDC
	b[2] = byte(compat >> 24)
	b[3] = byte(compat >> 16)
	b[4] = byte(compat >> 8)
	b[5] = byte(compat)
	copy(b[6:12], constraints[:])
	b[12] = level
	return b
}

// TestHEVCCodecStringMainProfile covers the plain (no profile-space letter,
// tier "L") path plus the trailing-zero-constraint trim loop.
func TestHEVCCodecStringMainProfile(t *testing.T) {
	hvcC := codecstrHVCC(0, 0, 1, 0x60000000, [6]byte{0xB0, 0, 0, 0, 0, 0}, 93)
	got := hevcCodecString(hvcC)
	want := "hvc1.1.6.L93.B0"
	if got != want {
		t.Errorf("hevcCodecString = %q, want %q", got, want)
	}
}

// TestHEVCCodecStringProfileSpaceLetterAndHighTier covers profileSpace > 0
// (the 'A'/'B'/'C' letter prefix), tier 1 ("H"), and the no-trim path through
// the constraint-byte loop (all six bytes non-zero, nothing trimmed).
func TestHEVCCodecStringProfileSpaceLetterAndHighTier(t *testing.T) {
	hvcC := codecstrHVCC(1, 1, 2, 0xF0000000, [6]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}, 150)
	got := hevcCodecString(hvcC)
	want := "hvc1.A2.F.H150.11.22.33.44.55.66"
	if got != want {
		t.Errorf("hevcCodecString = %q, want %q", got, want)
	}
}

// TestHEVCCodecStringTooShort covers the len(hvcC) < 13 guard.
func TestHEVCCodecStringTooShort(t *testing.T) {
	if got := hevcCodecString(make([]byte, 12)); got != "" {
		t.Errorf("hevcCodecString(12 bytes) = %q, want empty", got)
	}
}

// ─── codecstring.go: av1CodecString ─────────────────────────────────────────

// codecstrAV1C builds a minimal AV1CodecConfigurationRecord: only av1C[1] and
// av1C[2] are read by av1CodecString.
func codecstrAV1C(profile, level, tier, highBD, twelve byte) []byte {
	b := make([]byte, 3)
	b[1] = profile<<5 | level
	b[2] = tier<<7 | highBD<<6 | twelve<<5
	return b
}

// TestAV1CodecStringMainTierEightBit covers the default tier "M" and 8-bit
// depth path (no highBD).
func TestAV1CodecStringMainTierEightBit(t *testing.T) {
	av1C := codecstrAV1C(0, 1, 0, 0, 0)
	got := av1CodecString(av1C)
	want := "av01.0.01M.08"
	if got != want {
		t.Errorf("av1CodecString = %q, want %q", got, want)
	}
}

// TestAV1CodecStringHighTierTenBit covers the "H" tier and the highBD branch
// that yields 10-bit depth (twelve_bit=0).
func TestAV1CodecStringHighTierTenBit(t *testing.T) {
	av1C := codecstrAV1C(0, 4, 1, 1, 0)
	got := av1CodecString(av1C)
	want := "av01.0.04H.10"
	if got != want {
		t.Errorf("av1CodecString = %q, want %q", got, want)
	}
}

// TestAV1CodecStringTwelveBit covers the nested twelve_bit=1 branch (12-bit
// depth).
func TestAV1CodecStringTwelveBit(t *testing.T) {
	av1C := codecstrAV1C(0, 8, 0, 1, 1)
	got := av1CodecString(av1C)
	want := "av01.0.08M.12"
	if got != want {
		t.Errorf("av1CodecString = %q, want %q", got, want)
	}
}

// TestAV1CodecStringTooShort covers the len(av1C) < 3 guard.
func TestAV1CodecStringTooShort(t *testing.T) {
	if got := av1CodecString([]byte{0, 1}); got != "" {
		t.Errorf("av1CodecString(2 bytes) = %q, want empty", got)
	}
}

// ─── codecstring.go: rfc6381Codec vp9 case + hlsCodecsAttr wiring ───────────

// TestRFC6381CodecVP9 covers rfc6381Codec's "vp9" case, which is otherwise
// unreached (existing fixtures are h264/aac).
func TestRFC6381CodecVP9(t *testing.T) {
	// Bare 8-byte VPCodecConfigurationRecord: profile=0, level=10,
	// bitDepth<<4|chroma<<1|fullRange = 8<<4 = 0x80, plus fullRange bit = 1.
	rec := []byte{0, 10, 0x81, 1, 1, 1, 0, 0}
	ot := &outTrack{mkv: mkv.Track{Codec: "vp9", CodecPrivate: rec}}
	got := rfc6381Codec(ot)
	want := "vp09.00.10.08"
	if got != want {
		t.Errorf("rfc6381Codec(vp9) = %q, want %q", got, want)
	}
}

// TestHLSCodecsAttrHEVCAndH264 exercises hlsCodecsAttr end-to-end with a
// track whose codec string comes from hevcCodecString, proving the wiring
// from fragTrack through outTrack into rfc6381Codec.
func TestHLSCodecsAttrHEVCAndH264(t *testing.T) {
	h264 := &outTrack{mkv: mkv.Track{Codec: "h264", CodecPrivate: fakeAVCC}}
	hvcC := codecstrHVCC(0, 0, 1, 0x60000000, [6]byte{0xB0, 0, 0, 0, 0, 0}, 93)
	hevc := &outTrack{mkv: mkv.Track{Codec: "hevc", CodecPrivate: hvcC}}

	got := hlsCodecsAttr([]*fragTrack{{outTrack: h264}, {outTrack: hevc}})
	want := "avc1.64001F,hvc1.1.6.L93.B0"
	if got != want {
		t.Errorf("hlsCodecsAttr = %q, want %q", got, want)
	}
}

// TestHLSCodecsAttrUnknownCodecEmpty covers hlsCodecsAttr's "any unknown
// codec voids the whole attribute" rule.
func TestHLSCodecsAttrUnknownCodecEmpty(t *testing.T) {
	h264 := &outTrack{mkv: mkv.Track{Codec: "h264", CodecPrivate: fakeAVCC}}
	unknown := &outTrack{mkv: mkv.Track{Codec: "not-a-real-codec"}}
	got := hlsCodecsAttr([]*fragTrack{{outTrack: h264}, {outTrack: unknown}})
	if got != "" {
		t.Errorf("hlsCodecsAttr with unknown codec = %q, want empty", got)
	}
}

// ─── codec.go: vpcCRecord (FullBox-prefixed vs. bare forms) ─────────────────

// TestVpcCRecordFullBoxForm covers the FullBox-prefixed branch (len >= 12 and
// cp[0] <= 1): the 4-byte version/flags header is stripped.
func TestVpcCRecordFullBoxForm(t *testing.T) {
	cp := []byte{0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	got := vpcCRecord(cp)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if string(got) != string(want) {
		t.Errorf("vpcCRecord(FullBox) = % x, want % x", got, want)
	}
}

// TestVpcCRecordBareForm covers the bare-record branch (len >= 8 but < 12):
// the record is returned verbatim, not sliced.
func TestVpcCRecordBareForm(t *testing.T) {
	cp := []byte{9, 8, 7, 6, 5, 4, 3, 2}
	got := vpcCRecord(cp)
	if string(got) != string(cp) {
		t.Errorf("vpcCRecord(bare) = % x, want % x (verbatim)", got, cp)
	}
}

// TestVpcCRecordLongButNotFullBox covers the len >= 12 && cp[0] <= 1 guard's
// second half: a 12-byte record whose first byte is > 1 must NOT be treated
// as FullBox-prefixed (it falls to the bare-form branch and is returned
// whole, not sliced at offset 4).
func TestVpcCRecordLongButNotFullBox(t *testing.T) {
	cp := []byte{2, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	got := vpcCRecord(cp)
	if string(got) != string(cp) {
		t.Errorf("vpcCRecord(cp[0]=2, len 12) = % x, want % x (verbatim, not sliced)", got, cp)
	}
}

// TestVpcCRecordTooShort covers the nil fallthrough.
func TestVpcCRecordTooShort(t *testing.T) {
	if got := vpcCRecord([]byte{1, 2, 3}); got != nil {
		t.Errorf("vpcCRecord(3 bytes) = % x, want nil", got)
	}
}

// ─── codec.go: parseVP9FrameHeader profile-1/3 chroma switch ───────────────

// codecstrVP9Header builds a minimal VP9 keyframe uncompressed header
// (profile 1 or 3) exercising the chroma_subsampling switch: frame_marker,
// profile (low bit then high bit), show_existing_frame=0, frame_type=KEY,
// show_frame/error_resilient (ignored), sync code, color_space (!=7),
// color_range, chroma_subsampling_x, chroma_subsampling_y, reserved_zero.
func codecstrVP9Header(profile, sx, sy byte) []byte {
	var w bitWriter
	w.write(2, 2)                      // frame_marker
	w.write(uint32(profile&1), 1)      // profile low bit
	w.write(uint32((profile>>1)&1), 1) // profile high bit
	if profile == 3 {
		w.write(0, 1) // reserved_zero
	}
	w.write(0, 1)         // show_existing_frame
	w.write(0, 1)         // frame_type = KEY_FRAME
	w.write(0, 2)         // show_frame, error_resilient_mode
	w.write(0x498342, 24) // frame sync code
	if profile >= 2 {
		w.write(0, 1) // ten_or_twelve_bit
	}
	w.write(0, 3)          // color_space (0 != CS_RGB)
	w.write(0, 1)          // color_range
	w.write(uint32(sx), 1) // chroma_subsampling_x
	w.write(uint32(sy), 1) // chroma_subsampling_y
	w.write(0, 1)          // reserved_zero
	return w.bytes()
}

// TestParseVP9FrameHeaderChroma420 covers the "sx == 1 && sy == 1" case
// (4:2:0), reached only for profile 1/3.
func TestParseVP9FrameHeaderChroma420(t *testing.T) {
	h, err := parseVP9FrameHeader(codecstrVP9Header(1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if h.profile != 1 || h.chroma != 0 {
		t.Errorf("header = %+v, want profile 1, chroma 0 (4:2:0)", h)
	}
}

// TestParseVP9FrameHeaderChroma422 covers the "sx == 1" (sy == 0) case
// (4:2:2).
func TestParseVP9FrameHeaderChroma422(t *testing.T) {
	h, err := parseVP9FrameHeader(codecstrVP9Header(1, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if h.profile != 1 || h.chroma != 2 {
		t.Errorf("header = %+v, want profile 1, chroma 2 (4:2:2)", h)
	}
}

// TestParseVP9FrameHeaderChroma444Default covers the switch's default case
// (sx == 0) for the same profile-1/3 branch (4:4:4).
func TestParseVP9FrameHeaderChroma444Default(t *testing.T) {
	h, err := parseVP9FrameHeader(codecstrVP9Header(3, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if h.profile != 3 || h.chroma != 3 {
		t.Errorf("header = %+v, want profile 3, chroma 3 (4:4:4)", h)
	}
}

// ─── sample.go: reconstructTiming last-sample duration branches ────────────

// TestReconstructTimingLastDurMsScaled covers the lastDurMs > 0 branch with a
// non-identity media timescale, so a mutated scale() call or a mutated branch
// condition produces a visibly wrong value instead of one that happens to
// match the sibling branch.
func TestReconstructTimingLastDurMsScaled(t *testing.T) {
	s := []sample{
		{pts: 0, sync: true},
		{pts: 100},
		{pts: 250},
	}
	const mts = 48000 // != movieTimescale, so scale() actually multiplies/divides
	tim := reconstructTiming(s, 999, mts, 0)
	// dts scaled = [0, 4800, 12000]. durations[0]=4800, durations[1]=7200,
	// durations[2] (tail, overwritten) = scale(999) = 47952.
	want := []int64{4800, 7200, 47952}
	if len(tim.durations) != 3 || tim.durations[0] != want[0] || tim.durations[1] != want[1] || tim.durations[2] != want[2] {
		t.Errorf("durations = %v, want %v", tim.durations, want)
	}
	wantTotal := want[0] + want[1] + want[2]
	if tim.total != wantTotal {
		t.Errorf("total = %d, want %d", tim.total, wantTotal)
	}
}

// TestReconstructTimingLastDurFallbackScaled covers the "lastDurMs <= 0 &&
// n > 1" branch (reuse of the previous delta) with a non-identity timescale
// and non-uniform spacing, so the reused value is distinguishable from any
// other candidate index.
func TestReconstructTimingLastDurFallbackScaled(t *testing.T) {
	s := []sample{
		{pts: 0, sync: true},
		{pts: 50},
		{pts: 120},
	}
	const mts = 24000
	tim := reconstructTiming(s, 0, mts, 0)
	// scale factor = 24. dts scaled = [0, 1200, 2880].
	// durations[0] = 1200, durations[1] = 1680, durations[2] = reuse durations[1] = 1680.
	want := []int64{1200, 1680, 1680}
	if len(tim.durations) != 3 || tim.durations[0] != want[0] || tim.durations[1] != want[1] || tim.durations[2] != want[2] {
		t.Errorf("durations = %v, want %v", tim.durations, want)
	}
	if tim.total != 1200+1680+1680 {
		t.Errorf("total = %d, want %d", tim.total, 1200+1680+1680)
	}
}

// TestReconstructTimingSingleSampleDefault covers the "n <= 1" default branch
// (duration forced to 1) explicitly with lastDurMs == 0.
func TestReconstructTimingSingleSampleDefault(t *testing.T) {
	s := []sample{{pts: 5, sync: true}}
	tim := reconstructTiming(s, 0, movieTimescale, 0)
	if len(tim.durations) != 1 || tim.durations[0] != 1 {
		t.Errorf("durations = %v, want [1]", tim.durations)
	}
}
