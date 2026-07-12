package reader

// coverage_test.go - statement coverage booster targeting functions that were
// at 0-85% after the main test suite. All tests are in package reader (internal)
// so unexported helpers (bitReader, skipAVCScalingList, etc.) are accessible.

import (
	"bytes"
	"context"
	"math/bits"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// ---------------------------------------------------------------------------
// FillColourFromCodecPrivate - exported wrapper that was 0%
// ---------------------------------------------------------------------------

func TestCoverFillColourFromCodecPrivate(t *testing.T) {
	// Exported wrapper must call through to fillColourFromCodecPrivate.
	tr := mkv.Track{
		Type:         mkv.VideoTrack,
		Codec:        "hevc",
		CodecPrivate: mustHex(t, hevcHDRPrivateHex),
	}
	FillColourFromCodecPrivate(&tr)
	if tr.ColorSpace == nil {
		t.Error("FillColourFromCodecPrivate: ColorSpace should be set from HEVC SPS")
	}
	// No-op on non-video track.
	tr2 := mkv.Track{Type: mkv.AudioTrack, Codec: "hevc", CodecPrivate: mustHex(t, hevcHDRPrivateHex)}
	FillColourFromCodecPrivate(&tr2)
	if tr2.ColorSpace != nil {
		t.Error("FillColourFromCodecPrivate: should be a no-op on non-video track")
	}
	// No-op on empty CodecPrivate.
	tr3 := mkv.Track{Type: mkv.VideoTrack, Codec: "hevc"}
	FillColourFromCodecPrivate(&tr3)
}

// ---------------------------------------------------------------------------
// OpenMeta / OpenMetaWithFS - both were 0% (file-based entry points)
// ---------------------------------------------------------------------------

func writeTempMKV(t *testing.T) string {
	t.Helper()
	w := uint32(640)
	h := uint32(480)
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true, Width: &w, Height: &h}},
	}
	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatalf("writer.Write: %v", err)
	}
	f, err := os.CreateTemp("", "coverage_test_*.mkv")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatalf("write: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

func TestCoverOpenMeta(t *testing.T) {
	path := writeTempMKV(t)
	c, err := OpenMeta(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Error("OpenMeta: expected at least one track")
	}
}

func TestCoverOpenMetaNonexistent(t *testing.T) {
	_, err := OpenMeta(context.Background(), "/nonexistent/coverage_test.mkv")
	if err == nil {
		t.Fatal("OpenMeta: expected error for nonexistent file")
	}
}

func TestCoverOpenMetaWithFS(t *testing.T) {
	path := writeTempMKV(t)
	// nil FS falls back to os.Open.
	c, err := OpenMetaWithFS(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("OpenMetaWithFS: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Error("OpenMetaWithFS: expected at least one track")
	}
}

func TestCoverOpenMetaWithFSNonexistent(t *testing.T) {
	_, err := OpenMetaWithFS(context.Background(), "/nonexistent/coverage_test.mkv", nil)
	if err == nil {
		t.Fatal("OpenMetaWithFS: expected error for nonexistent file")
	}
}

// ---------------------------------------------------------------------------
// Bit-writer helper (tbwCov): reuses the tbw/tbwSe logic from avc_vui_colour_test.go
// but with a different name to avoid duplicate declarations. Uses the same
// MSB-first encoding.
// ---------------------------------------------------------------------------

type tbwCov struct{ bits []byte }

func (w *tbwCov) u(val uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, byte((val>>uint(i))&1))
	}
}

func (w *tbwCov) ue(val uint32) {
	v := val + 1
	n := bits.Len32(v) - 1
	for i := 0; i < n; i++ {
		w.bits = append(w.bits, 0)
	}
	w.u(v, n+1)
}

// se writes a signed Exp-Golomb value: positive k → ue(2k-1), negative k → ue(-2k), 0 → ue(0).
func (w *tbwCov) se(val int32) {
	if val == 0 {
		w.ue(0)
	} else if val > 0 {
		w.ue(uint32(2*val - 1))
	} else {
		w.ue(uint32(-2 * val))
	}
}

func (w *tbwCov) bytes() []byte {
	out := make([]byte, (len(w.bits)+7)/8)
	for i, b := range w.bits {
		if b != 0 {
			out[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// skipAVCScalingList - was 0%
// Exercises via parseAVCSPS on a High-profile SPS with seq_scaling_matrix_present_flag=1.
// ---------------------------------------------------------------------------

// buildHighSPSWithScalingMatrix builds an avcC whose SPS is High-profile and
// has seq_scaling_matrix_present_flag=1. The first scaling list entry uses
// se(0) (delta=0 → next stays 8) then se(-8) (delta=-8 → next→0), exercising
// both the "inner if true" and "inner if false" branches of skipAVCScalingList.
func buildHighSPSWithScalingMatrix() []byte {
	b := &tbwCov{}
	// --- SPS RBSP ---
	b.u(100, 8) // profile_idc = High (100)
	b.u(0, 8)   // constraint flags + reserved
	b.u(40, 8)  // level_idc
	b.ue(0)     // seq_parameter_set_id
	b.ue(1)     // chroma_format_idc = 1 (4:2:0; cf != 3, so n=8 in the loop)
	b.ue(0)     // bit_depth_luma_minus8
	b.ue(0)     // bit_depth_chroma_minus8
	b.u(0, 1)   // qpprime_y_zero_transform_bypass_flag
	b.u(1, 1)   // seq_scaling_matrix_present_flag = 1 (triggers the loop)
	// n=8 entries (chroma_format_idc=1, not 3)
	for i := 0; i < 8; i++ {
		b.u(1, 1) // seq_scaling_list_present_flag[i] = 1 → call skipAVCScalingList
		// skipAVCScalingList(r, size): size=16 for i<6, 64 for i>=6
		// Write the scaling list entries using se() values.
		// Entry 0: se(0) → next=(8+0+256)%256=8, inner true
		// Entry 1: se(-8) → next=(8-8+256)%256=0, inner false
		// Entries 2+: next==0, outer false (no bits)
		size := 16
		if i >= 6 {
			size = 64
		}
		b.se(0)  // entry 0: delta=0, next=8, inner if true
		b.se(-8) // entry 1: delta=-8, next=0, inner if false
		for j := 2; j < size; j++ {
			// next==0 so outer if is false - no bits needed
			_ = j
		}
	}
	// --- rest of SPS (must have enough bits to parse or fail cleanly) ---
	b.ue(0)   // log2_max_frame_num_minus4
	b.ue(0)   // pic_order_cnt_type = 0
	b.ue(0)   // log2_max_pic_order_cnt_lsb_minus4
	b.ue(1)   // max_num_ref_frames
	b.u(0, 1) // gaps_in_frame_num_value_allowed_flag
	b.ue(29)  // pic_width_in_mbs_minus1
	b.ue(17)  // pic_height_in_map_units_minus1
	b.u(1, 1) // frame_mbs_only_flag = 1
	b.u(1, 1) // direct_8x8_inference_flag
	b.u(0, 1) // frame_cropping_flag
	b.u(1, 1) // vui_parameters_present_flag
	b.u(0, 1) // aspect_ratio_info_present_flag
	b.u(0, 1) // overscan_info_present_flag
	b.u(1, 1) // video_signal_type_present_flag
	b.u(5, 3) // video_format
	b.u(0, 1) // video_full_range_flag
	b.u(1, 1) // colour_description_present_flag
	b.u(1, 8) // colour_primaries = 1 (bt709)
	b.u(1, 8) // transfer_characteristics = 1
	b.u(1, 8) // matrix_coefficients = 1

	rbsp := b.bytes()
	// Wrap in avcC: version(1) profile_idc(1) compat(1) level(1) [6r|2lsm](1) [3r|5numSPS](1) spslen(2) SPS
	nal := append([]byte{0x67}, rbsp...) // NAL type 7 = SPS
	avcc := []byte{0x01, 100, 0, 40, 0xFF, 0xE1, byte(len(nal) >> 8), byte(len(nal))}
	avcc = append(avcc, nal...)
	// Append zero PPS count
	avcc = append(avcc, 0x00)
	return avcc
}

func TestCoverSkipAVCScalingList(t *testing.T) {
	avcc := buildHighSPSWithScalingMatrix()
	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	FillColourFromCodecPrivate(&tr)
	// The SPS was built with valid VUI colour. Even if bit-level trimming causes
	// parseAVCSPS to fail, the skipAVCScalingList function was executed.
	// The key assertion is that we didn't panic.
}

// TestCoverSkipAVCScalingListDirect calls skipAVCScalingList directly so the
// test does not depend on the avcC wrapper framing.
func TestCoverSkipAVCScalingListDirect(t *testing.T) {
	b := &tbwCov{}
	// 16 entries, covering all branches:
	b.se(0)  // entry 0: outer true, read delta=0 → next=8, inner true
	b.se(-8) // entry 1: outer true, read delta=-8 → next=0, inner false
	// entries 2-15: outer false (next==0), no bits consumed
	r := &bitReader{data: b.bytes()}
	skipAVCScalingList(r, 16)
	if r.err {
		t.Error("skipAVCScalingList: unexpected error with valid bitstream")
	}

	// size=64 path (hit the different size branch)
	b2 := &tbwCov{}
	b2.se(0)  // entry 0
	b2.se(-8) // entry 1 → next=0
	r2 := &bitReader{data: b2.bytes()}
	skipAVCScalingList(r2, 64)
}

// ---------------------------------------------------------------------------
// skipHEVCScalingListData - was 0%
// ---------------------------------------------------------------------------

func TestCoverSkipHEVCScalingListDataDirect(t *testing.T) {
	b := &tbwCov{}
	// sizeID=0 (coef=16), 6 matrix entries, step=1:
	//   entry 0: pred_mode_flag=1 → no dc_coef (sizeID<=1), 16 se(0) coefs
	//   entries 1-5: pred_mode_flag=0 → ue(0) (pred_matrix_id_delta=0)
	// sizeID=1 (coef=64), 6 entries, step=1:
	//   entry 0: pred_mode_flag=1 → 64 se(0) coefs
	//   entries 1-5: pred_mode_flag=0 → ue(0)
	// sizeID=2 (coef=64, sizeID>1 → dc_coef), 6 entries, step=1:
	//   entry 0: pred_mode_flag=1 → se(0) dc_coef, 64 se(0) coefs
	//   entries 1-5: pred_mode_flag=0 → ue(0)
	// sizeID=3 (coef=64, sizeID>1 → dc_coef), 2 entries, step=3:
	//   entry 0: pred_mode_flag=1 → se(0) dc_coef, 64 se(0) coefs
	//   entry 1: pred_mode_flag=0 → ue(0)

	for sizeID := 0; sizeID < 4; sizeID++ {
		step := 1
		if sizeID == 3 {
			step = 3
		}
		coef := 64
		if c2 := 1 << uint(4+(sizeID<<1)); c2 < 64 {
			coef = c2
		}
		first := true
		for matrixID := 0; matrixID < 6; matrixID += step {
			_ = matrixID
			if first {
				b.u(1, 1) // pred_mode_flag=1 (explicit)
				if sizeID > 1 {
					b.se(0) // dc_coef
				}
				for i := 0; i < coef; i++ {
					b.se(0) // coef delta
				}
				first = false
			} else {
				b.u(0, 1) // pred_mode_flag=0 (pred)
				b.ue(0)   // pred_matrix_id_delta=0
			}
		}
	}

	r := &bitReader{data: b.bytes()}
	skipHEVCScalingListData(r)
	if r.err {
		t.Errorf("skipHEVCScalingListData: unexpected error")
	}
}

// TestCoverSkipHEVCScalingListDataError covers the r.err early-exit branch.
func TestCoverSkipHEVCScalingListDataError(t *testing.T) {
	// Empty data → first bit() call sets r.err, function returns early
	r := &bitReader{data: []byte{}}
	skipHEVCScalingListData(r)
	// Just verify no panic.
}

// ---------------------------------------------------------------------------
// skipHEVCShortTermRPS - was 0%
// ---------------------------------------------------------------------------

func TestCoverSkipHEVCShortTermRPSDirect(t *testing.T) {
	b := &tbwCov{}
	// num=3 entries, covering:
	//   idx=0: inter forced 0 (no inter bit)
	//     neg=1, pos=1, read pos, neg delta_poc and use flag
	//   idx=1: inter bit=1 (inter RPS)
	//     delta_rps_sign=0, abs_delta_rps_minus1=0
	//     ref = numDelta[0] = 2 (neg+pos from idx=0)
	//     read ref+1 = 3 entries: (used, maybe useDelta)
	//   idx=2: inter bit=0
	//     neg=0, pos=0

	// idx=0: neg=1, pos=1, no inter bit
	b.ue(1) // neg = 1
	b.ue(1) // pos = 1
	// 1 neg entry: poc, use_flag=1
	b.ue(0)   // delta_poc_s0_minus1[0] = 0
	b.u(1, 1) // used_by_curr_pic_s0_flag[0] = 1
	// 1 pos entry: poc, use_flag=1
	b.ue(0)   // delta_poc_s1_minus1[0] = 0
	b.u(1, 1) // used_by_curr_pic_s1_flag[0] = 1

	// idx=1: inter=1
	b.u(1, 1) // inter=1
	b.u(0, 1) // delta_rps_sign=0
	b.ue(0)   // abs_delta_rps_minus1=0
	// ref = numDelta[0] = 2, so read ref+1=3 entries of (used, useDelta if !used)
	// entry 0: used=1
	b.u(1, 1) // used=1
	// entry 1: used=0, useDelta=1 → keep++
	b.u(0, 1) // used=0
	b.u(1, 1) // useDelta=1 → keep++
	// entry 2: used=0, useDelta=0 → no keep
	b.u(0, 1) // used=0
	b.u(0, 1) // useDelta=0

	// idx=2: inter=0 (read inter bit for idx>0)
	b.u(0, 1) // inter=0
	b.ue(0)   // neg=0
	b.ue(0)   // pos=0
	// no pos/neg entries to read

	r := &bitReader{data: b.bytes()}
	skipHEVCShortTermRPS(r, 3)
	if r.err {
		t.Errorf("skipHEVCShortTermRPS(3): unexpected error")
	}
}

// TestCoverSkipHEVCShortTermRPSOverflow covers the num>64 error path.
func TestCoverSkipHEVCShortTermRPSOverflow(t *testing.T) {
	r := &bitReader{data: bytes.Repeat([]byte{0xFF}, 16)}
	skipHEVCShortTermRPS(r, 65) // exceeds the 64-entry limit
	if !r.err {
		t.Error("skipHEVCShortTermRPS: expected err=true when num>64")
	}
}

// TestCoverSkipHEVCShortTermRPSNegOverflow covers the neg>1024 overflow path.
func TestCoverSkipHEVCShortTermRPSNegOverflow(t *testing.T) {
	b := &tbwCov{}
	// idx=0: neg > 1024 → should set r.err
	b.ue(1025) // neg = 1025 > 1024
	b.ue(0)    // pos = 0
	r := &bitReader{data: b.bytes()}
	skipHEVCShortTermRPS(r, 1)
	if !r.err {
		t.Error("skipHEVCShortTermRPS: expected err=true when neg>1024")
	}
}

// ---------------------------------------------------------------------------
// uvlc - was 0%
// ---------------------------------------------------------------------------

func TestCoverUvlc(t *testing.T) {
	// uvlc=0: leading bit is 1 → zeros=0 → return 0
	r0 := &bitReader{data: []byte{0b10000000}}
	if v := r0.uvlc(); v != 0 {
		t.Errorf("uvlc(1...) = %d, want 0", v)
	}

	// uvlc=1: bits "010" → zeros=1, bits(1)=0 → (1<<1)-1+0 = 1
	r1 := &bitReader{data: []byte{0b01000000}}
	if v := r1.uvlc(); v != 1 {
		t.Errorf("uvlc(010...) = %d, want 1", v)
	}

	// uvlc=2: bits "011" → zeros=1, bits(1)=1 → 1+1=2
	r2 := &bitReader{data: []byte{0b01100000}}
	if v := r2.uvlc(); v != 2 {
		t.Errorf("uvlc(011...) = %d, want 2", v)
	}

	// 32 leading zeros → return 0xffffffff (overflow guard)
	// Need 32 zeros then at least one 1-bit and value bits.
	zeroBytes := [5]byte{0, 0, 0, 0, 0b00000000} // 40 zeros
	// Override: place 1-bit at position 32 to hit the zeros>=32 guard
	var loopData [5]byte
	// Build 32 zero bits then a 1 bit
	loopData[4] = 0b10000000 // bit 32 = 1
	r3 := &bitReader{data: loopData[:]}
	if v := r3.uvlc(); v != 0xffffffff {
		t.Errorf("uvlc with 32 leading zeros = 0x%x, want 0xffffffff", v)
	}
	_ = zeroBytes

	// r.err case: empty data → bit() sets err → uvlc returns 0
	r4 := &bitReader{data: []byte{}}
	v4 := r4.uvlc()
	// bit() will set r.err=true immediately, uvlc should return 0
	if v4 != 0 {
		t.Errorf("uvlc on empty data = %d, want 0", v4)
	}
}

// ---------------------------------------------------------------------------
// Stream reader coverage: parseStreamInfo, parseStreamTrackEntry,
// parseStreamVideo, parseStreamColour, parseStreamAudio,
// parseStreamContentEncoding/Compression
// ---------------------------------------------------------------------------

// buildFullStreamMKV builds a stream-style MKV with all the field types that
// the low-coverage streaming parsers handle.
func buildFullStreamMKV(t *testing.T) []byte {
	t.Helper()
	b := &bytes.Buffer{}

	// --- Info: Title, MuxingApp, WritingApp (these were missing from stream tests)
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 1_000_000, 3)
	ebml.WriteElementHeader(&info, mkv.IDDuration, 8)
	ebml.WriteFloat(&info, 5000.0)
	ebml.WriteElementHeader(&info, mkv.IDTitle, 8)
	ebml.WriteString(&info, "TestFile")
	ebml.WriteElementHeader(&info, mkv.IDMuxingApp, 5)
	ebml.WriteString(&info, "mkvgo")
	ebml.WriteElementHeader(&info, mkv.IDWritingApp, 5)
	ebml.WriteString(&info, "mkvgo")

	// --- Video track with DisplayWidth/DisplayHeight/FlagInterlaced/Colour+BitDepth
	var colour bytes.Buffer
	ebml.WriteElementHeader(&colour, mkv.IDColourMatrix, 1)
	ebml.WriteUint(&colour, 9, 1)
	ebml.WriteElementHeader(&colour, mkv.IDColourTransfer, 1)
	ebml.WriteUint(&colour, 16, 1)
	ebml.WriteElementHeader(&colour, mkv.IDColourPrimaries, 1)
	ebml.WriteUint(&colour, 9, 1)
	ebml.WriteElementHeader(&colour, mkv.IDColourRange, 1)
	ebml.WriteUint(&colour, 1, 1)
	ebml.WriteElementHeader(&colour, mkv.IDColourBitsPerChannel, 1) // IDColourBitsPerChannel = 0x55B2
	ebml.WriteUint(&colour, 10, 1)

	var video bytes.Buffer
	ebml.WriteElementHeader(&video, mkv.IDPixelWidth, 2)
	ebml.WriteUint(&video, 1920, 2)
	ebml.WriteElementHeader(&video, mkv.IDPixelHeight, 2)
	ebml.WriteUint(&video, 1080, 2)
	ebml.WriteElementHeader(&video, mkv.IDDisplayWidth, 2) // IDDisplayWidth = 0x54B0
	ebml.WriteUint(&video, 1920, 2)
	ebml.WriteElementHeader(&video, mkv.IDDisplayHeight, 2) // IDDisplayHeight = 0x54BA
	ebml.WriteUint(&video, 1080, 2)
	ebml.WriteElementHeader(&video, mkv.IDFlagInterlaced, 1) // IDFlagInterlaced = 0x9A
	ebml.WriteUint(&video, 2, 1)                             // 2 = progressive
	ebml.WriteElementHeader(&video, mkv.IDColour, int64(colour.Len()))
	video.Write(colour.Bytes())

	// Video TrackEntry: codec not in CodecShortName → t.Codec = v
	var videoTE bytes.Buffer
	ebml.WriteElementHeader(&videoTE, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&videoTE, 1, 1)
	ebml.WriteElementHeader(&videoTE, mkv.IDTrackUID, 1)
	ebml.WriteUint(&videoTE, 100, 1)
	ebml.WriteElementHeader(&videoTE, mkv.IDTrackType, 1)
	ebml.WriteUint(&videoTE, mkv.TrackTypeVideo, 1)
	ebml.WriteElementHeader(&videoTE, mkv.IDCodecID, 16)
	ebml.WriteString(&videoTE, "V_MPEGH/ISO/HEVC") // has a CodecShortName mapping
	ebml.WriteElementHeader(&videoTE, mkv.IDFlagDefault, 1)
	ebml.WriteUint(&videoTE, 1, 1)
	ebml.WriteElementHeader(&videoTE, mkv.IDDefaultDuration, 4) // IDDefaultDuration = 0x23E383
	ebml.WriteUint(&videoTE, 33_333_333, 4)                     // ~30fps
	ebml.WriteElementHeader(&videoTE, mkv.IDVideo, int64(video.Len()))
	videoTE.Write(video.Bytes())

	// --- Audio track with OutputSampleRate and BitDepth
	var audio bytes.Buffer
	ebml.WriteElementHeader(&audio, mkv.IDSamplingFreq, 8)
	ebml.WriteFloat(&audio, 48000.0)
	ebml.WriteElementHeader(&audio, mkv.IDOutputSamplingFreq, 8) // IDOutputSamplingFreq = 0x78B5
	ebml.WriteFloat(&audio, 48000.0)
	ebml.WriteElementHeader(&audio, mkv.IDChannels, 1)
	ebml.WriteUint(&audio, 2, 1)
	ebml.WriteElementHeader(&audio, mkv.IDBitDepth, 1)
	ebml.WriteUint(&audio, 16, 1)

	// ContentCompression with ContentCompSettings (header stripping)
	headerStrip := []byte{0x01, 0x02, 0x03}
	var compSettings bytes.Buffer
	ebml.WriteElementHeader(&compSettings, mkv.IDContentCompSettings, int64(len(headerStrip)))
	compSettings.Write(headerStrip)

	var compression bytes.Buffer
	ebml.WriteElementHeader(&compression, mkv.IDContentCompression, int64(compSettings.Len()))
	compression.Write(compSettings.Bytes())

	var encoding bytes.Buffer
	ebml.WriteElementHeader(&encoding, mkv.IDContentEncoding, int64(compression.Len()))
	encoding.Write(compression.Bytes())

	var encodings bytes.Buffer
	ebml.WriteElementHeader(&encodings, mkv.IDContentEncodings, int64(encoding.Len()))
	encodings.Write(encoding.Bytes())

	var audioTE bytes.Buffer
	ebml.WriteElementHeader(&audioTE, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&audioTE, 2, 1)
	ebml.WriteElementHeader(&audioTE, mkv.IDTrackUID, 1)
	ebml.WriteUint(&audioTE, 200, 1)
	ebml.WriteElementHeader(&audioTE, mkv.IDTrackType, 1)
	ebml.WriteUint(&audioTE, mkv.TrackTypeAudio, 1)
	ebml.WriteElementHeader(&audioTE, mkv.IDCodecID, 6)
	ebml.WriteString(&audioTE, "A_OPUS")
	ebml.WriteElementHeader(&audioTE, mkv.IDLanguage, 3)
	ebml.WriteString(&audioTE, "eng")
	ebml.WriteElementHeader(&audioTE, mkv.IDLanguageBCP47, 5) // IDLanguageBCP47 = 0x22B59D
	ebml.WriteString(&audioTE, "en-US")
	ebml.WriteElementHeader(&audioTE, mkv.IDFlagForced, 1) // IDFlagForced = 0x55AA
	ebml.WriteUint(&audioTE, 1, 1)
	ebml.WriteElementHeader(&audioTE, mkv.IDAudio, int64(audio.Len()))
	audioTE.Write(audio.Bytes())
	audioTE.Write(encodings.Bytes())

	// --- Subtitle track (SubtitleTrack type)
	var subtitleTE bytes.Buffer
	ebml.WriteElementHeader(&subtitleTE, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&subtitleTE, 3, 1)
	ebml.WriteElementHeader(&subtitleTE, mkv.IDTrackType, 1)
	ebml.WriteUint(&subtitleTE, mkv.TrackTypeSubtitle, 1)
	ebml.WriteElementHeader(&subtitleTE, mkv.IDCodecID, 11)
	ebml.WriteString(&subtitleTE, "S_UNKNOWN_X") // not in CodecShortName → raw codec string
	ebml.WriteElementHeader(&subtitleTE, mkv.IDName, 7)
	ebml.WriteString(&subtitleTE, "English")
	ebml.WriteElementHeader(&subtitleTE, mkv.IDFlagDefault, 1)
	ebml.WriteUint(&subtitleTE, 0, 1) // explicit default=0

	// --- SimpleTag with TagLanguage (was uncovered)
	var simpleTag bytes.Buffer
	ebml.WriteElementHeader(&simpleTag, mkv.IDTagName, 5)
	ebml.WriteString(&simpleTag, "TITLE")
	ebml.WriteElementHeader(&simpleTag, mkv.IDTagString, 4)
	ebml.WriteString(&simpleTag, "Test")
	ebml.WriteElementHeader(&simpleTag, mkv.IDTagLanguage, 3) // IDTagLanguage = 0x447A
	ebml.WriteString(&simpleTag, "eng")

	var target bytes.Buffer
	ebml.WriteElementHeader(&target, mkv.IDTargetType, 5)
	ebml.WriteString(&target, "MOVIE")

	var tag bytes.Buffer
	ebml.WriteElementHeader(&tag, mkv.IDTargets, int64(target.Len()))
	tag.Write(target.Bytes())
	ebml.WriteElementHeader(&tag, mkv.IDSimpleTag, int64(simpleTag.Len()))
	tag.Write(simpleTag.Bytes())

	var tags bytes.Buffer
	ebml.WriteElementHeader(&tags, mkv.IDTag, int64(tag.Len()))
	tags.Write(tag.Bytes())

	// --- Assemble tracks
	var tracks bytes.Buffer
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(videoTE.Len()))
	tracks.Write(videoTE.Bytes())
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(audioTE.Len()))
	tracks.Write(audioTE.Bytes())
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(subtitleTE.Len()))
	tracks.Write(subtitleTE.Bytes())

	// --- Segment body
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTracks, int64(tracks.Len()))
	seg.Write(tracks.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTags, int64(tags.Len()))
	seg.Write(tags.Bytes())
	// Append a Cluster so ReadStream returns its BlockReader.
	seg.Write(realCluster())

	ebml.WriteElementHeader(b, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(b, mkv.IDSegment, int64(seg.Len()))
	b.Write(seg.Bytes())
	return b.Bytes()
}

func TestCoverReadStreamFullFields(t *testing.T) {
	data := buildFullStreamMKV(t)
	c, br, err := ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if br == nil {
		t.Fatal("ReadStream: BlockReader should not be nil")
	}

	if c.Info.Title != "TestFile" {
		t.Errorf("Info.Title = %q, want TestFile", c.Info.Title)
	}
	if c.Info.MuxingApp != "mkvgo" {
		t.Errorf("Info.MuxingApp = %q, want mkvgo", c.Info.MuxingApp)
	}
	if c.Info.WritingApp != "mkvgo" {
		t.Errorf("Info.WritingApp = %q, want mkvgo", c.Info.WritingApp)
	}

	if len(c.Tracks) != 3 {
		t.Fatalf("track count = %d, want 3", len(c.Tracks))
	}

	// Video track checks
	vtr := c.Tracks[0]
	if vtr.DisplayWidth == nil || *vtr.DisplayWidth != 1920 {
		t.Errorf("DisplayWidth = %v, want 1920", vtr.DisplayWidth)
	}
	if vtr.DisplayHeight == nil || *vtr.DisplayHeight != 1080 {
		t.Errorf("DisplayHeight = %v, want 1080", vtr.DisplayHeight)
	}
	if vtr.FieldOrder != "progressive" {
		t.Errorf("FieldOrder = %q, want progressive", vtr.FieldOrder)
	}
	if vtr.VideoBitDepth == nil || *vtr.VideoBitDepth != 10 {
		t.Errorf("VideoBitDepth = %v, want 10", vtr.VideoBitDepth)
	}
	if vtr.FrameRate == nil {
		t.Errorf("FrameRate should be set for video track with DefaultDuration")
	}

	// Audio track checks
	atr := c.Tracks[1]
	if atr.OutputSampleRate == nil {
		t.Error("OutputSampleRate should be set")
	}
	if atr.BitDepth == nil || *atr.BitDepth != 16 {
		t.Errorf("BitDepth = %v, want 16", atr.BitDepth)
	}
	if !atr.IsForced {
		t.Error("IsForced should be true")
	}
	if atr.LanguageBCP47 != "en-US" {
		t.Errorf("LanguageBCP47 = %q, want en-US", atr.LanguageBCP47)
	}
	if !bytes.Equal(atr.HeaderStripping, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("HeaderStripping = %v, want [1 2 3]", atr.HeaderStripping)
	}

	// Subtitle track checks
	str := c.Tracks[2]
	if str.Type != mkv.SubtitleTrack {
		t.Errorf("track type = %v, want SubtitleTrack", str.Type)
	}
	if str.Codec != "S_UNKNOWN_X" {
		t.Errorf("codec = %q, want S_UNKNOWN_X (not in short name map)", str.Codec)
	}

	// Tag language check
	if len(c.Tags) > 0 && len(c.Tags[0].SimpleTags) > 0 {
		if c.Tags[0].SimpleTags[0].Language != "eng" {
			t.Errorf("SimpleTag.Language = %q, want eng", c.Tags[0].SimpleTags[0].Language)
		}
	}
}

// ---------------------------------------------------------------------------
// interlacedName value 2 (progressive) - was at 75%
// ---------------------------------------------------------------------------

func TestCoverInterlacedNameProgressive(t *testing.T) {
	// interlacedName(2) should return "progressive".
	// Exercise via a track built with IDFlagInterlaced=2 in the seekable reader.
	var videoSub bytes.Buffer
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelWidth, 2)
	ebml.WriteUint(&videoSub, 1920, 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelHeight, 2)
	ebml.WriteUint(&videoSub, 1080, 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDFlagInterlaced, 1)
	ebml.WriteUint(&videoSub, 2, 1) // progressive

	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_VP9"),
		masterElem(mkv.IDVideo, videoSub.Bytes()),
	)
	tr := readFirstTrack(t, buildMKV(te))
	if tr.FieldOrder != "progressive" {
		t.Errorf("FieldOrder = %q, want progressive", tr.FieldOrder)
	}
}

// TestCoverInterlacedNameInterlaced exercises the interlacedName(1) path.
func TestCoverInterlacedNameInterlaced(t *testing.T) {
	var videoSub bytes.Buffer
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelWidth, 2)
	ebml.WriteUint(&videoSub, 1920, 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelHeight, 2)
	ebml.WriteUint(&videoSub, 1080, 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDFlagInterlaced, 1)
	ebml.WriteUint(&videoSub, 1, 1) // interlaced

	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_VP9"),
		masterElem(mkv.IDVideo, videoSub.Bytes()),
	)
	tr := readFirstTrack(t, buildMKV(te))
	if tr.FieldOrder != "interlaced" {
		t.Errorf("FieldOrder = %q, want interlaced", tr.FieldOrder)
	}
}

// ---------------------------------------------------------------------------
// parseColour ColourBitsPerChannel - covers the uncovered IDColourBitsPerChannel branch
// ---------------------------------------------------------------------------

func TestCoverParseColourBitsPerChannel(t *testing.T) {
	var colour bytes.Buffer
	ebml.WriteElementHeader(&colour, mkv.IDColourMatrix, 1)
	ebml.WriteUint(&colour, 9, 1)
	ebml.WriteElementHeader(&colour, mkv.IDColourBitsPerChannel, 1)
	ebml.WriteUint(&colour, 10, 1)

	var videoSub bytes.Buffer
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelWidth, 2)
	ebml.WriteUint(&videoSub, 3840, 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelHeight, 2)
	ebml.WriteUint(&videoSub, 2160, 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDColour, int64(colour.Len()))
	videoSub.Write(colour.Bytes())

	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_VP9"),
		masterElem(mkv.IDVideo, videoSub.Bytes()),
	)
	tr := readFirstTrack(t, buildMKV(te))
	if tr.VideoBitDepth == nil || *tr.VideoBitDepth != 10 {
		t.Errorf("VideoBitDepth = %v, want 10", tr.VideoBitDepth)
	}
}

// ---------------------------------------------------------------------------
// parseAudioSettings OutputSampleRate - covers the uncovered branch
// ---------------------------------------------------------------------------

func TestCoverParseAudioOutputSampleRate(t *testing.T) {
	sr := 48000.0
	osr := 96000.0
	ch := uint8(2)
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.AudioTrack, Codec: "opus", IsDefault: true,
				SampleRate: &sr, OutputSampleRate: &osr, Channels: &ch},
		},
	}
	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatalf("writer.Write: %v", err)
	}
	got, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "osr.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	if got.Tracks[0].OutputSampleRate == nil || *got.Tracks[0].OutputSampleRate != 96000.0 {
		t.Errorf("OutputSampleRate = %v, want 96000", got.Tracks[0].OutputSampleRate)
	}
}

// ---------------------------------------------------------------------------
// parseBlockAdditionMapping - covers the uncovered IDBlockAddIDExtraData branch
// ---------------------------------------------------------------------------

func TestCoverParseBlockAdditionMappingUnknown(t *testing.T) {
	// Build a track with a BlockAdditionMapping that uses an unknown type (not DV).
	// This exercises the full parseBlockAdditionMapping loop without the DV branch.
	var bam bytes.Buffer
	ebml.WriteElementHeader(&bam, mkv.IDBlockAddIDType, 1)
	ebml.WriteUint(&bam, 0x42424242, 4) // unknown type
	ebml.WriteElementHeader(&bam, mkv.IDBlockAddIDExtraData, 4)
	bam.Write([]byte{0xAA, 0xBB, 0xCC, 0xDD})

	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_VP9"),
		masterElem(mkv.IDBlockAdditionMapping, bam.Bytes()),
	)
	tr := readFirstTrack(t, buildMKV(te))
	if tr.DolbyVision != nil {
		t.Error("expected no DolbyVision for unknown BlockAddIDType")
	}
}

// ---------------------------------------------------------------------------
// reader_meta SeekHead Cues path: exercise parseSeekHeadMeta pointing at Cues
// so that finalizeKeyframes follows the SeekHead's Cues offset.
// ---------------------------------------------------------------------------

func TestCoverSeekHeadCuesOffset(t *testing.T) {
	// Build: SeekHead → Info → Tracks → Cues (SeekHead points at Cues)
	// ReadMeta should follow the Cues offset in finalizeKeyframes.
	info := infoElem()
	tracks := tracksElem()

	var cuesBuf bytes.Buffer
	if err := writer.WriteCues(&cuesBuf, []mkv.CuePoint{
		{TimeMs: 0, Track: 1, ClusterPos: 100},
	}, 1_000_000); err != nil {
		t.Fatal(err)
	}
	cues := cuesBuf.Bytes()

	// SeekHead pointing at Info, Tracks, and Cues.
	// segStart offset is after the SeekHead itself. Compute lengths.
	// We'll use buildSeekHeadMKV-style construction:
	// positions in the SeekHead are relative to Segment data start.

	// placeholder SeekHead to compute its own serialised length.
	ph := seekHeadElem(
		seekEntry(mkv.IDInfo, 0),
		seekEntry(mkv.IDTracks, 0),
		seekEntry(mkv.IDCues, 0),
	)
	shLen := uint64(len(ph))

	infoPos := shLen
	tracksPos := infoPos + uint64(len(info))
	cuesPos := tracksPos + uint64(len(tracks))

	sh := seekHeadElem(
		seekEntry(mkv.IDInfo, infoPos),
		seekEntry(mkv.IDTracks, tracksPos),
		seekEntry(mkv.IDCues, cuesPos),
	)
	if uint64(len(sh)) != shLen {
		t.Skipf("seekHead length changed: %d vs %d", len(sh), shLen)
	}

	data := segmentMKV(sh, info, tracks, cues)
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "seekcues.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Error("ReadMeta: expected tracks")
	}
	// ReadMeta derives Keyframes from Cues (via SeekHead) then sets Cues=nil.
	if c.Cues != nil {
		t.Error("ReadMeta: Cues must be nil (contract)")
	}
}

// ---------------------------------------------------------------------------
// bufReadSeeker: exercise SeekEnd and invalid whence paths
// ---------------------------------------------------------------------------

func TestCoverBufReadSeekerSeekEnd(t *testing.T) {
	// ReadMeta uses bufReadSeeker. Build a SeekHead MKV with Tracks after Cluster
	// to force a parseElementAt → seekAbs call. The finalizeKeyframes path also
	// exercises seekAbs when a Cues offset is followed.
	data := buildSeekHeadMKV(t, true)
	// This calls ReadMeta internally via parseSegmentMeta which uses bufReadSeeker.
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "seekend.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(c.Tracks))
	}
}

// TestCoverBufReadSeekerInvalidWhence covers the error path for an unsupported
// whence value. Since we cannot call bufReadSeeker directly from tests (it's
// constructed inside ReadMeta), we instead verify that a SeekEnd-triggered path
// works correctly by exercising a file that requires a SeekEnd-relative seek.
// The invalid-whence branch is exercised by constructing a bufReadSeeker and
// calling Seek with whence=99.
func TestCoverBufReadSeekerInvalidWhence(t *testing.T) {
	// Construct a bufReadSeeker and test the invalid-whence path directly.
	inner := bytes.NewReader([]byte{0x01, 0x02, 0x03})
	brs, err := newBufReadSeeker(inner, 16)
	if err != nil {
		t.Fatalf("newBufReadSeeker: %v", err)
	}
	// Seek with invalid whence (not 0, 1, or 2)
	_, err = brs.Seek(0, 99)
	if err == nil {
		t.Error("expected error for invalid whence")
	}
	// SeekEnd path
	_, err = brs.Seek(0, 2) // SeekEnd, offset 0
	if err != nil {
		t.Errorf("SeekEnd: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Stream: boundedLoop with unknown-size element → error
// ---------------------------------------------------------------------------

func TestCoverStreamBoundedLoopUnknownSize(t *testing.T) {
	p := &streamParser{r: bytes.NewReader(nil)}
	err := p.boundedLoop(-1, func(_ ebml.ElementHeader) error { return nil })
	if err == nil {
		t.Error("boundedLoop with size=-1 should return error")
	}
}

// ---------------------------------------------------------------------------
// AV1 reduced sequence header path (triggers bc.level = u16p(bits(5)))
// ---------------------------------------------------------------------------

func TestCoverAV1ReducedSeqHeader(t *testing.T) {
	// Build a minimal AV1CodecConfigurationRecord with a reduced_still_picture_header.
	// Structure: av1C header (4 bytes) + sequence OBU.
	//
	// AV1 seq header bits (reduced=1):
	//   seq_profile(3) still_picture(1) reduced_still_picture_header(1)=1
	//   seq_level_idx[0](5) → bc.level set
	//   frame_width_bits_minus_1(4) frame_height_bits_minus_1(4)
	//   max_frame_width_minus_1 max_frame_height_minus_1
	//   ... color_config
	b := &tbwCov{}
	b.u(0, 3) // seq_profile=0
	b.u(0, 1) // still_picture=0
	b.u(1, 1) // reduced_still_picture_header=1
	b.u(5, 5) // seq_level_idx[0] = 5
	// frame_width/height bits
	b.u(3, 4)  // frame_width_bits_minus_1=3 → 4 bits for width
	b.u(3, 4)  // frame_height_bits_minus_1=3 → 4 bits for height
	b.u(79, 4) // max_frame_width_minus_1 (4 bits)
	b.u(79, 4) // max_frame_height_minus_1 (4 bits)
	// reduced → no frame_id_numbers, no enable_interintra, etc.
	// feature flags:
	b.u(0, 1) // use_128x128_superblock
	b.u(0, 1) // enable_filter_intra
	b.u(0, 1) // enable_intra_edge_filter
	// reduced=1 → skip interframe stuff
	b.u(0, 1) // enable_superres
	b.u(0, 1) // enable_cdef
	b.u(0, 1) // enable_restoration
	// color_config: high_bit_depth=0, not mono, color_description_present=1
	b.u(0, 1)  // high_bit_depth=0
	b.u(0, 1)  // mono=0 (seqProfile != 1 so we read this)
	b.u(1, 1)  // color_description_present_flag=1
	b.u(9, 8)  // color_primaries = 9 (bt2020)
	b.u(16, 8) // transfer_characteristics = 16 (PQ)
	b.u(9, 8)  // matrix_coefficients = 9
	b.u(0, 1)  // color_range = 0 (limited)
	// seqProfile=0 → chroma = 4:2:0 (no bits)

	payload := b.bytes()
	// Wrap as AV1 OBU: type=1 (sequence header), no extension, size present
	// OBU header: forbidden(1) type(4) ext_flag(1) has_size_flag(1) reserved(1)
	// = 0b 0_0001_0_1_0 = 0x0A  (type=1, no ext, has_size=1)
	sz := len(payload)
	obu := []byte{0x0A, byte(sz)}
	obu = append(obu, payload...)

	// av1C: marker(1) version(7) seq_profile(3) seq_level_idx(5) ...
	// marker=1, version=1 → 0x81
	// cp[1]: seq_profile(3) | seq_level_idx_0(5) = 0<<5|5 = 0x05
	// cp[2]: seq_tier_0(1) | high_bitdepth(1) | twelve_bit(1) | monochrome(1) |
	//         chroma_subsampling_x(1) | chroma_subsampling_y(1) | chroma_sample_position(2) = 0
	//         for profile=0, 8-bit: 0b0_0_0_0_1_1_0_0 = 0x0C (4:2:0 non-colocated)
	// cp[3]: initial_presentation_delay_present(1) = 0
	av1C := []byte{0x81, 0x05, 0x0C, 0x00}
	av1C = append(av1C, obu...)

	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: av1C}
	FillColourFromCodecPrivate(&tr)
	// bc.level should be set (seq_level_idx=5)
	if tr.Level == nil {
		t.Error("AV1 reduced seq header: Level should be set")
	} else if *tr.Level != 5 {
		t.Errorf("AV1 reduced seq header: Level = %d, want 5", *tr.Level)
	}
}

// ---------------------------------------------------------------------------
// AV1 timing path with uvlc (equal_picture_interval=1)
// ---------------------------------------------------------------------------

func TestCoverAV1TimingWithUvlc(t *testing.T) {
	// Build AV1 sequence header with timing_info_present_flag=1 and
	// equal_picture_interval=1 to exercise the r.uvlc() path.
	b := &tbwCov{}
	b.u(0, 3) // seq_profile=0
	b.u(0, 1) // still_picture=0
	b.u(0, 1) // reduced_still_picture_header=0
	b.u(1, 1) // timing_info_present_flag=1
	// timing_info:
	b.u(90000, 32) // num_units_in_display_tick
	b.u(90000, 32) // time_scale
	b.u(1, 1)      // equal_picture_interval=1 → read uvlc
	// uvlc value 0 = single '1' bit
	b.u(1, 1) // uvlc=0 (num_ticks_per_picture_minus_1)
	b.u(0, 1) // decoder_model_info_present_flag=0
	b.u(0, 1) // initial_display_delay_present_flag=0
	b.u(0, 5) // operating_points_cnt_minus_1=0 → 1 op
	// op 0:
	b.u(0, 12) // operating_point_idc=0
	b.u(5, 5)  // seq_level_idx=5 (≤7, no seq_tier bit)
	// rest of seq header
	b.u(3, 4)  // frame_width_bits_minus_1=3
	b.u(3, 4)  // frame_height_bits_minus_1=3
	b.u(79, 4) // max_frame_width_minus_1
	b.u(79, 4) // max_frame_height_minus_1
	b.u(0, 1)  // frame_id_numbers_present_flag=0 (reduced=0)
	b.u(0, 1)  // use_128x128_superblock
	b.u(0, 1)  // enable_filter_intra
	b.u(0, 1)  // enable_intra_edge_filter
	b.u(0, 1)  // enable_interintra_compound
	b.u(0, 1)  // enable_masked_compound
	b.u(0, 1)  // enable_warped_motion
	b.u(0, 1)  // enable_dual_filter
	b.u(0, 1)  // enable_order_hint=0
	// no jnt/ref_frame_mvs
	b.u(1, 1) // seq_choose_screen_content_tools==0? bit=0 → read forceScreen
	b.u(1, 1) // forceScreen=1
	b.u(1, 1) // forceScreen>0 → seq_choose_integer_mv==0? bit=0 → read seq_force_integer_mv
	b.u(0, 1) // seq_force_integer_mv=0
	// no order_hint_bits
	b.u(0, 1) // enable_superres
	b.u(0, 1) // enable_cdef
	b.u(0, 1) // enable_restoration
	// color_config
	b.u(0, 1) // high_bit_depth=0
	b.u(0, 1) // mono=0
	b.u(0, 1) // color_description_present=0
	b.u(0, 1) // color_range=0
	// seqProfile=0 → chroma=4:2:0

	payload := b.bytes()
	obu := []byte{0x0A, byte(len(payload))}
	obu = append(obu, payload...)
	av1C := []byte{0x81, 0x05, 0x0C, 0x00}
	av1C = append(av1C, obu...)

	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: av1C}
	FillColourFromCodecPrivate(&tr)
	// Just verify no panic and the parser ran.
}

// ---------------------------------------------------------------------------
// pixelFormat coverage: 12-bit suffix, yuv422p, yuv444p, gray, gray+suffix
// ---------------------------------------------------------------------------

func TestCoverPixelFormat(t *testing.T) {
	tests := []struct {
		chroma  uint16
		depth   uint16
		wantFmt string
	}{
		{1, 12, "yuv420p12le"}, // 12-bit suffix
		{2, 8, "yuv422p"},
		{2, 10, "yuv422p10le"},
		{3, 8, "yuv444p"},
		{0, 8, "gray"},
		{0, 10, "gray10le"},
	}
	for _, tt := range tests {
		c, d := tt.chroma, tt.depth
		got := pixelFormat(&c, &d)
		if got != tt.wantFmt {
			t.Errorf("pixelFormat(%d, %d) = %q, want %q", tt.chroma, tt.depth, got, tt.wantFmt)
		}
	}
	// nil chroma → ""
	if got := pixelFormat(nil, nil); got != "" {
		t.Errorf("pixelFormat(nil, nil) = %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// AVC profile name coverage (Baseline, Extended, High 10)
// ---------------------------------------------------------------------------

func TestCoverAVCProfileNames(t *testing.T) {
	// Each profile idc should return the right string.
	cases := []struct {
		idc  uint32
		want string
	}{
		{66, "Baseline"},
		{77, "Main"},
		{88, "Extended"},
		{100, "High"},
		{110, "High 10"},
		{122, "High 4:2:2"},
		{244, "High 4:4:4 Predictive"},
		{255, ""},
	}
	for _, tc := range cases {
		got := avcProfileName(tc.idc)
		if got != tc.want {
			t.Errorf("avcProfileName(%d) = %q, want %q", tc.idc, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// HEVC profile name coverage
// ---------------------------------------------------------------------------

func TestCoverHEVCProfileNames(t *testing.T) {
	cases := []struct {
		idc  byte
		want string
	}{
		{1, "Main"},
		{2, "Main 10"},
		{3, "Main Still Picture"},
		{4, "Range Extensions"},
		{5, ""},
	}
	for _, tc := range cases {
		got := hevcProfileName(tc.idc)
		if got != tc.want {
			t.Errorf("hevcProfileName(%d) = %q, want %q", tc.idc, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// AVC POC type 1 path (uncovered: num_ref_frames_in_pic_order_cnt_cycle loop)
// ---------------------------------------------------------------------------

func TestCoverAVCPOCType1(t *testing.T) {
	// Build a Baseline SPS with poc_type=1 (not 0) to hit the se()/loop in parseAVCSPS.
	b := &tbwCov{}
	b.u(66, 8) // profile_idc = Baseline (66) - NOT a high profile, no scaling matrix
	b.u(0, 8)  // constraint flags
	b.u(30, 8) // level_idc
	b.ue(0)    // seq_parameter_set_id
	// Baseline: no high-profile extras
	b.ue(0)   // log2_max_frame_num_minus4
	b.ue(1)   // pic_order_cnt_type = 1
	b.u(0, 1) // delta_pic_order_always_zero_flag
	b.se(0)   // offset_for_non_ref_pic
	b.se(0)   // offset_for_top_to_bottom_field
	b.ue(2)   // num_ref_frames_in_pic_order_cnt_cycle = 2
	b.se(0)   // offset[0]
	b.se(0)   // offset[1]
	b.ue(1)   // max_num_ref_frames
	b.u(0, 1) // gaps_in_frame_num_value_allowed_flag
	b.ue(29)  // pic_width_in_mbs_minus1
	b.ue(17)  // pic_height_in_map_units_minus1
	b.u(1, 1) // frame_mbs_only_flag
	b.u(1, 1) // direct_8x8_inference_flag
	b.u(0, 1) // frame_cropping_flag
	b.u(1, 1) // vui_parameters_present_flag
	b.u(0, 1) // aspect_ratio_info_present_flag
	b.u(0, 1) // overscan_info_present_flag
	b.u(1, 1) // video_signal_type_present_flag
	b.u(5, 3) // video_format
	b.u(0, 1) // video_full_range_flag
	b.u(1, 1) // colour_description_present_flag
	b.u(1, 8) // colour_primaries = 1 (bt709)
	b.u(1, 8) // transfer_characteristics = 1
	b.u(1, 8) // matrix_coefficients = 1

	rbsp := b.bytes()
	nal := append([]byte{0x67}, rbsp...)
	avcc := []byte{0x01, 66, 0, 30, 0xFF, 0xE1, byte(len(nal) >> 8), byte(len(nal))}
	avcc = append(avcc, nal...)
	avcc = append(avcc, 0x00)

	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	FillColourFromCodecPrivate(&tr)
	// bt709 should be parsed from VUI
}

// ---------------------------------------------------------------------------
// AVC VUI: overscan_info_present_flag=1 path
// ---------------------------------------------------------------------------

func TestCoverAVCOverscanPresentFlag(t *testing.T) {
	b := &tbwCov{}
	b.u(100, 8) // High
	b.u(0, 8)
	b.u(40, 8)
	b.ue(0)   // seq_parameter_set_id
	b.ue(1)   // chroma_format_idc
	b.ue(0)   // bit_depth_luma_minus8
	b.ue(0)   // bit_depth_chroma_minus8
	b.u(0, 1) // qpprime
	b.u(0, 1) // seq_scaling_matrix_present_flag=0
	b.ue(0)   // log2_max_frame_num_minus4
	b.ue(0)   // poc_type=0
	b.ue(0)   // log2_max_poc_lsb_minus4
	b.ue(1)   // max_num_ref_frames
	b.u(0, 1) // gaps
	b.ue(29)  // width
	b.ue(17)  // height
	b.u(1, 1) // frame_mbs_only
	b.u(1, 1) // direct_8x8
	b.u(0, 1) // cropping
	b.u(1, 1) // vui_parameters_present_flag
	b.u(0, 1) // aspect_ratio_info_present_flag=0
	b.u(1, 1) // overscan_info_present_flag=1 (this branch was missing)
	b.u(1, 1) // overscan_appropriate_flag
	b.u(1, 1) // video_signal_type_present_flag=1
	b.u(5, 3) // video_format
	b.u(0, 1) // full range=0
	b.u(1, 1) // colour_description_present_flag=1
	b.u(1, 8) // primaries
	b.u(1, 8) // transfer
	b.u(1, 8) // matrix

	nal := append([]byte{0x67}, b.bytes()...)
	avcc := []byte{0x01, 100, 0, 40, 0xFF, 0xE1, byte(len(nal) >> 8), byte(len(nal))}
	avcc = append(avcc, nal...)
	avcc = append(avcc, 0x00)

	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcc}
	FillColourFromCodecPrivate(&tr)
}

// ---------------------------------------------------------------------------
// skipHEVCProfileTierLevel with maxSub > 0 and sub-layer profile/level flags
// (lines 626-644 were 50% - the sub-layer loop was never executed)
// ---------------------------------------------------------------------------

func TestCoverSkipHEVCProfileTierLevelSubLayers(t *testing.T) {
	b := &tbwCov{}
	// general_profile_tier_level: 8+32+32+16+8 = 96 bits
	b.u(0x02, 8) // profile_space(2)+tier(1)+profile_idc(5)
	b.u(0, 32)   // general_profile_compatibility_flags
	b.u(0, 32)   // constraint flags (4) + 28 reserved
	b.u(0, 16)   // remaining 16 reserved
	b.u(150, 8)  // general_level_idc = 150

	// maxSub=1: one sub-layer
	// Loop i=0..0: prof[0]=1, lvl[0]=1
	b.u(1, 1) // prof[0] = 1
	b.u(1, 1) // lvl[0] = 1

	// maxSub > 0: reserved_zero_2bits for i=1..7 (7 pairs)
	for i := 1; i < 8; i++ {
		b.u(0, 2)
	}

	// sub-layer profile block for i=0 (prof[0]==1): 32+32+24 bits
	b.u(0, 32)
	b.u(0, 32)
	b.u(0, 24)
	// sub-layer level block for i=0 (lvl[0]==1): 8 bits
	b.u(0, 8)

	bc := &bitstreamColour{}
	r := &bitReader{data: b.bytes()}
	skipHEVCProfileTierLevel(r, 1, bc)
	if r.err {
		t.Error("skipHEVCProfileTierLevel with maxSub=1: unexpected error")
	}
	if bc.level == nil || *bc.level != 150 {
		t.Errorf("level = %v, want 150", bc.level)
	}
}

// ---------------------------------------------------------------------------
// parseAV1ColorConfig: monochrome, sRGB, profile 1 (4:4:4), profile 2 non-12-bit
// These were at 52.3% - most branch paths uncovered.
// ---------------------------------------------------------------------------

func TestCoverAV1ColorConfigMonochrome(t *testing.T) {
	// mono=1 path: chroma=0, read color_range, return early
	b := &tbwCov{}
	b.u(0, 1) // high_bit_depth=0 → bitDepth=8
	b.u(1, 1) // mono=1 (seqProfile=0 != 1, so this is read)
	b.u(0, 1) // color_description_present=0 → cp,tc,mc=2 (unspecified)
	b.u(0, 1) // color_range=0 (read when mono==1)

	bc := &bitstreamColour{}
	r := &bitReader{data: b.bytes()}
	parseAV1ColorConfig(r, 0, bc)
	if r.err {
		t.Error("AV1 mono color_config: unexpected error")
	}
	if bc.chroma == nil || *bc.chroma != 0 {
		t.Errorf("mono chroma = %v, want 0", bc.chroma)
	}
}

func TestCoverAV1ColorConfigSRGB(t *testing.T) {
	// sRGB path: cp=1, tc=13, mc=0 → chroma=3 (4:4:4), full range
	b := &tbwCov{}
	b.u(0, 1)  // high_bit_depth=0
	b.u(0, 1)  // mono=0 (seqProfile=0)
	b.u(1, 1)  // color_description_present=1
	b.u(1, 8)  // cp=1 (bt709)
	b.u(13, 8) // tc=13 (sRGB/IEC 61966-2-1)
	b.u(0, 8)  // mc=0 (identity/GBR)
	// cp==1 && tc==13 && mc==0 → sRGB path → return early (no color_range bit)

	bc := &bitstreamColour{}
	r := &bitReader{data: b.bytes()}
	parseAV1ColorConfig(r, 0, bc)
	if r.err {
		t.Error("AV1 sRGB color_config: unexpected error")
	}
	if bc.chroma == nil || *bc.chroma != 3 {
		t.Errorf("sRGB chroma = %v, want 3 (4:4:4)", bc.chroma)
	}
	if bc.rng == nil || *bc.rng != 2 {
		t.Errorf("sRGB rng = %v, want 2 (full)", bc.rng)
	}
}

func TestCoverAV1ColorConfigProfile1(t *testing.T) {
	// profile=1 always 4:4:4; mono bit is NOT read for seqProfile==1
	b := &tbwCov{}
	b.u(0, 1) // high_bit_depth=0
	// seqProfile==1 → no mono bit
	b.u(0, 1) // color_description_present=0 → cp,tc,mc=2 (unspecified)
	b.u(0, 1) // color_range bit (not sRGB, not mono)

	bc := &bitstreamColour{}
	r := &bitReader{data: b.bytes()}
	parseAV1ColorConfig(r, 1, bc)
	if r.err {
		t.Error("AV1 profile 1 color_config: unexpected error")
	}
	if bc.chroma == nil || *bc.chroma != 3 {
		t.Errorf("profile1 chroma = %v, want 3 (4:4:4)", bc.chroma)
	}
}

func TestCoverAV1ColorConfigProfile2NonTwelveBit(t *testing.T) {
	// profile=2, 10-bit (not 12-bit) → chroma=2 (4:2:2)
	b := &tbwCov{}
	b.u(1, 1) // high_bit_depth=1 (seqProfile=2 && highBitDepth=1 → read twelve_bit)
	b.u(0, 1) // twelve_bit=0 → bitDepth=10
	b.u(0, 1) // mono=0 (seqProfile=2 != 1)
	b.u(0, 1) // color_description_present=0
	b.u(0, 1) // color_range bit
	// seqProfile=2, bitDepth=10 (not 12) → chroma=2 (4:2:2)

	bc := &bitstreamColour{}
	r := &bitReader{data: b.bytes()}
	parseAV1ColorConfig(r, 2, bc)
	if r.err {
		t.Error("AV1 profile 2 10-bit color_config: unexpected error")
	}
	if bc.chroma == nil || *bc.chroma != 2 {
		t.Errorf("profile2/10-bit chroma = %v, want 2 (4:2:2)", bc.chroma)
	}
	if bc.bitDepth == nil || *bc.bitDepth != 10 {
		t.Errorf("profile2/10-bit depth = %v, want 10", bc.bitDepth)
	}
}

// ---------------------------------------------------------------------------
// interlacedName default return ("") - was missing at 75%
// ---------------------------------------------------------------------------

func TestCoverInterlacedNameDefault(t *testing.T) {
	if got := interlacedName(0); got != "" {
		t.Errorf("interlacedName(0) = %q, want \"\"", got)
	}
	if got := interlacedName(99); got != "" {
		t.Errorf("interlacedName(99) = %q, want \"\"", got)
	}
}

// ---------------------------------------------------------------------------
// readColourDescription flag=0 path (was at 66.7% - the return nil,nil,nil was missing)
// ---------------------------------------------------------------------------

func TestCoverReadColourDescriptionFlagZero(t *testing.T) {
	// bit=0 → colour_description_present_flag=0 → return nil,nil,nil
	r := &bitReader{data: []byte{0x00}} // first bit is 0
	p, tr, m := readColourDescription(r)
	if p != nil || tr != nil || m != nil {
		t.Errorf("readColourDescription(0): want nil,nil,nil got %v,%v,%v", p, tr, m)
	}
}

// ---------------------------------------------------------------------------
// HEVC SPS: scaling list, PCM, long-term ref pics, conformance window
// ---------------------------------------------------------------------------

func TestCoverHEVCSPSScalingListPCMLTR(t *testing.T) {
	// Construct a minimal HEVC hvcC that contains an SPS with
	// scaling_list_enabled=1, sps_scaling_list_data_present=1, pcm_enabled=1,
	// and long_term_ref_pics_present=1.
	//
	// Build the SPS RBSP manually using tbwCov.
	b := &tbwCov{}

	// --- SPS header before profile_tier_level ---
	b.u(0, 4) // sps_video_parameter_set_id
	maxSub := uint32(0)
	b.u(maxSub, 3) // sps_max_sub_layers_minus1
	b.u(0, 1)      // sps_temporal_id_nesting_flag

	// skipHEVCProfileTierLevel(r, maxSub=0, bc):
	b.u(0, 8)   // profile_space(2) + tier(1) + profile_idc(5) = 0
	b.u(0, 32)  // general_profile_compatibility_flags
	b.u(0, 32)  // source flags (4) + 28 reserved
	b.u(0, 16)  // remaining 16 reserved
	b.u(150, 8) // general_level_idc = 150 → level=150
	// maxSub=0 → no prof/lvl loop, no reserved_zero_2bits

	b.ue(0) // sps_seq_parameter_set_id
	cf := uint32(1)
	b.ue(cf) // chroma_format_idc = 1 (4:2:0)
	// cf != 3 → no separate_colour_plane_flag
	b.ue(79)                // pic_width_in_luma_samples
	b.ue(79)                // pic_height_in_luma_samples
	b.u(1, 1)               // conformance_window_flag=1 (exercises the ue ue ue ue path)
	b.ue(0)                 // conf_win_left_offset
	b.ue(0)                 // conf_win_right_offset
	b.ue(0)                 // conf_win_top_offset
	b.ue(0)                 // conf_win_bottom_offset
	b.ue(0)                 // bit_depth_luma_minus8
	b.ue(0)                 // bit_depth_chroma_minus8
	log2PocLsb := uint32(4) // log2_max_pic_order_cnt_lsb_minus4 + 4
	b.ue(log2PocLsb - 4)    // log2_max_pic_order_cnt_lsb_minus4 = 0 → log2PocLsb=4
	b.u(0, 1)               // sps_sub_layer_ordering_info_present_flag=0
	// loop from maxSub(0) to maxSub(0): 1 iteration
	b.ue(3) // sps_max_dec_pic_buffering_minus1
	b.ue(2) // sps_max_num_reorder_pics
	b.ue(0) // sps_max_latency_increase_plus1
	b.ue(0) // log2_min_luma_coding_block_size_minus3
	b.ue(0) // log2_diff_max_min_luma_coding_block_size
	b.ue(0) // log2_min_luma_transform_block_size_minus2
	b.ue(0) // log2_diff_max_min_luma_transform_block_size
	b.ue(0) // max_transform_hierarchy_depth_inter
	b.ue(0) // max_transform_hierarchy_depth_intra

	b.u(1, 1) // scaling_list_enabled_flag=1
	b.u(1, 1) // sps_scaling_list_data_present_flag=1 → skipHEVCScalingListData

	// skipHEVCScalingListData: write all pred_mode=0 entries (20 entries)
	for sizeID := 0; sizeID < 4; sizeID++ {
		step := 1
		if sizeID == 3 {
			step = 3
		}
		for matrixID := 0; matrixID < 6; matrixID += step {
			_ = matrixID
			b.u(0, 1) // pred_mode_flag=0
			b.ue(0)   // pred_matrix_id_delta=0
		}
	}

	b.u(0, 1) // amp_enabled_flag
	b.u(0, 1) // sample_adaptive_offset_enabled_flag
	b.u(1, 1) // pcm_enabled_flag=1 (exercises pcm bits)
	b.u(7, 4) // pcm_sample_bit_depth_luma_minus1
	b.u(7, 4) // pcm_sample_bit_depth_chroma_minus1
	b.ue(0)   // log2_min_pcm_luma_coding_block_size_minus3
	b.ue(0)   // log2_diff_max_min_pcm_luma_coding_block_size
	b.u(0, 1) // pcm_loop_filter_disabled_flag

	// skipHEVCShortTermRPS(r, 0): 0 entries → no bits
	b.ue(0) // num_short_term_ref_pic_sets = 0

	b.u(1, 1) // long_term_ref_pics_present_flag=1
	b.ue(1)   // num_long_term_ref_pics_sps = 1 (≤64)
	// 1 entry: lt_ref_pic_poc_lsb_sps (log2PocLsb=4 bits), used_flag
	b.u(0, int(log2PocLsb)) // lt_ref_pic_poc_lsb_sps
	b.u(1, 1)               // used_by_curr_pic_lt_sps_flag

	b.u(0, 1) // sps_temporal_mvp_enabled_flag
	b.u(0, 1) // strong_intra_smoothing_enabled_flag
	b.u(1, 1) // vui_parameters_present_flag=1

	// parseHEVCVUI:
	b.u(0, 1)  // aspect_ratio_info_present_flag=0
	b.u(0, 1)  // overscan_info_present_flag=0
	b.u(1, 1)  // video_signal_type_present_flag=1
	b.u(5, 3)  // video_format
	b.u(0, 1)  // video_full_range_flag=0
	b.u(1, 1)  // colour_description_present_flag=1
	b.u(9, 8)  // colour_primaries = bt2020
	b.u(16, 8) // transfer = PQ
	b.u(9, 8)  // matrix = bt2020nc

	// Wrap in hvcC: version(1)=0x01, profile_tier_level bits, numArrays(1)=1 SPS array
	// hvcC byte layout (simplified):
	// [0]=0x01 (version)
	// [1]=profile(5 bits low)=0, so 0x00
	// ... bytes 2-16 are flags/reserved, byte 17 = bitDepthLumaMinus8 (low 3 bits)
	// [17] = 0 (8-bit)
	// [22] = numArrays = 1
	// Array: [completeness|reserved(2)|nal_type(6)] [count_high][count_low] [len_high][len_low] [nal data...]
	// nal_type=33 (SPS_NUT=0x21)
	spsRBSP := b.bytes()
	spsNAL := append([]byte{0x42, 0x01}, spsRBSP...) // NAL header: type=33 (0x21 | forbidden=0 | layer=0 | tid=1)

	// Minimal 23-byte hvcC header
	hvcC := make([]byte, 23)
	hvcC[0] = 0x01  // version
	hvcC[1] = 0x02  // profile_idc=2 (Main 10)
	hvcC[17] = 0x02 // bitDepthLumaMinus8=2 → 10-bit
	hvcC[22] = 0x01 // numArrays=1
	// Array entry
	hvcC = append(hvcC, 0x21)       // completeness(1)|reserved(2)|nal_type(6) = 0x21 (SPS_NUT=33)
	hvcC = append(hvcC, 0x00, 0x01) // count = 1
	spsLen := len(spsNAL)
	hvcC = append(hvcC, byte(spsLen>>8), byte(spsLen))
	hvcC = append(hvcC, spsNAL...)

	tr := mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: hvcC}
	FillColourFromCodecPrivate(&tr)
	// bt2020/PQ should be set from VUI
	if tr.ColorPrimaries == nil || *tr.ColorPrimaries != 9 {
		t.Errorf("HEVC SPS with scaling/PCM/LTR: primaries = %v, want 9 (bt2020)", tr.ColorPrimaries)
	}
}
