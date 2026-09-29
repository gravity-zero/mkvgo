package reader

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// vp9KeyframeHeaderBytes builds the start of a VP9 keyframe's uncompressed
// header for a profile (0 or 2), a color_space and a range flag - 4:2:0 for
// both profiles, 10-bit for profile 2.
func vp9KeyframeHeaderBytes(profile uint8, colorSpace uint8, fullRange bool) []byte {
	var bits []uint32
	push := func(v uint32, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>uint(i))&1)
		}
	}
	push(2, 2)                      // frame_marker
	push(uint32(profile&1), 1)      // profile_low_bit
	push(uint32((profile>>1)&1), 1) // profile_high_bit
	push(0, 1)                      // show_existing_frame
	push(0, 1)                      // frame_type = KEY_FRAME
	push(1, 1)                      // show_frame
	push(0, 1)                      // error_resilient_mode
	push(0x498342, 24)              // frame_sync_code
	if profile >= 2 {
		push(0, 1) // ten_or_twelve_bit = 0 → 10-bit
	}
	push(uint32(colorSpace), 3)
	if colorSpace != 7 {
		if fullRange {
			push(1, 1)
		} else {
			push(0, 1)
		}
	}
	push(0, 16) // padding (frame size etc., not parsed)
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		out[i/8] |= byte(b << uint(7-i%8))
	}
	return out
}

// vp9MKV is a one-track VP9 file: no CodecPrivate unless cp is given, an
// optional container Colour element, and one keyframe block.
func vp9MKV(cp []byte, colour []byte, w, h uint64, frame []byte) []byte {
	video := []([]byte){uintElem(mkv.IDPixelWidth, w, 2), uintElem(mkv.IDPixelHeight, h, 2)}
	if colour != nil {
		video = append(video, colour)
	}
	elems := []([]byte){
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_VP9"),
	}
	if cp != nil {
		elems = append(elems, bytesElem(mkv.IDCodecPrivate, cp))
	}
	elems = append(elems, masterElem(mkv.IDVideo, video...))
	te := trackEntry(elems...)
	info := masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4))
	tracks := masterElem(mkv.IDTracks, te)

	block := append([]byte{0x81, 0x00, 0x00, 0x80}, frame...) // track 1, relTC 0, keyframe
	cluster := clusterWithSimpleBlock(block)

	var seg bytes.Buffer
	seg.Write(info)
	seg.Write(tracks)
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(len(cluster)))
	seg.Write(cluster)

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

func u16v(p *uint16) int {
	if p == nil {
		return -1
	}
	return int(*p)
}

func TestParseVP9KeyframeHeaderColorSpace(t *testing.T) {
	h, err := ParseVP9KeyframeHeader(vp9KeyframeHeaderBytes(2, 5, true))
	if err != nil {
		t.Fatal(err)
	}
	if h.Profile != 2 || h.BitDepth != 10 || h.Chroma != 0 || h.ColorSpace != 5 || !h.FullRange {
		t.Errorf("header = %+v, want profile 2, 10-bit, 4:2:0, BT.2020, full range", h)
	}
	h, err = ParseVP9KeyframeHeader(vp9KeyframeHeaderBytes(0, 7, false))
	if err != nil {
		t.Fatal(err)
	}
	if h.ColorSpace != 7 || !h.FullRange || h.Chroma != 3 {
		t.Errorf("sRGB header = %+v, want CS 7, full range, 4:4:4", h)
	}
}

// TestVP9LevelDerivedHeadOnly: a VP9 track with no vpcC gets its level from
// the picture size on the plain head-only read - the same level the MP4 remux
// declares - and no profile (that lives in the keyframe, not the header).
func TestVP9LevelDerivedHeadOnly(t *testing.T) {
	data := vp9MKV(nil, nil, 2048, 858, vp9KeyframeHeaderBytes(0, 2, false))
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.webm")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if u16v(tr.Level) != 40 || tr.Profile != "" || tr.ColorTransfer != nil {
		t.Errorf("head-only: level=%d profile=%q transfer=%v, want 40 / \"\" / nil", u16v(tr.Level), tr.Profile, tr.ColorTransfer)
	}
	// Same on the full read and the stream path.
	if c, err := Read(context.Background(), bytes.NewReader(data), "x.webm"); err != nil || u16v(c.Tracks[0].Level) != 40 {
		t.Errorf("Read: level=%d err=%v, want 40", u16v(c.Tracks[0].Level), err)
	}
	if c, _, err := ReadStream(context.Background(), &readerOnly{r: bytes.NewReader(data)}); err != nil || u16v(c.Tracks[0].Level) != 40 {
		t.Errorf("ReadStream: err=%v, want level 40", err)
	}
}

// TestVP9InBandHeader: with the in-band option the keyframe header fills the
// profile, bit depth, pixel format, range and the colour its color_space
// fixes; the container Colour element always wins per field.
func TestVP9InBandHeader(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name                                           string
		profile, cs                                    uint8
		full                                           bool
		colour                                         []byte
		wantProfile, wantPix                           string
		wantDepth, wantPrim, wantTx, wantMx, wantRange int
		wantDetermined                                 bool
	}{
		{"profile 0 bt709", 0, 2, false, nil, "Profile 0", "yuv420p", 8, 1, 1, 1, 1, true},
		{"profile 2 bt2020 leaves transfer open", 2, 5, false, nil, "Profile 2", "yuv420p10le", 10, 9, -1, 9, 1, true},
		{"bt601 fixes matrix and transfer only", 0, 1, false, nil, "Profile 0", "yuv420p", 8, -1, 6, 5, 1, true},
		{"srgb", 0, 7, true, nil, "Profile 0", "yuv444p", 8, 1, 13, 0, 2, true},
		{"unknown fills nothing", 0, 0, true, nil, "Profile 0", "yuv420p", 8, -1, -1, -1, 2, false},
		{"container transfer wins", 2, 5, false,
			masterElem(mkv.IDColour, uintElem(mkv.IDColourTransfer, 16, 1)),
			"Profile 2", "yuv420p10le", 10, 9, 16, 9, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := vp9MKV(nil, tc.colour, 1920, 1080, vp9KeyframeHeaderBytes(tc.profile, tc.cs, tc.full))
			base, err := ReadMeta(ctx, bytes.NewReader(data), "x.webm")
			if err != nil {
				t.Fatal(err)
			}
			if base.Tracks[0].Profile != "" {
				t.Fatalf("without the option the profile must stay empty, got %q", base.Tracks[0].Profile)
			}
			c, err := ReadMeta(ctx, bytes.NewReader(data), "x.webm", WithInBandColourFallback())
			if err != nil {
				t.Fatal(err)
			}
			tr := c.Tracks[0]
			got := []int{u16v(tr.VideoBitDepth), u16v(tr.ColorPrimaries), u16v(tr.ColorTransfer), u16v(tr.ColorSpace), u16v(tr.ColorRange)}
			want := []int{tc.wantDepth, tc.wantPrim, tc.wantTx, tc.wantMx, tc.wantRange}
			if tr.Profile != tc.wantProfile || tr.PixelFormat != tc.wantPix || u16v(tr.Level) != 40 ||
				got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] || got[4] != want[4] ||
				tr.ColourDetermined != tc.wantDetermined {
				t.Errorf("profile=%q pix=%q level=%d depth/prim/tx/mx/range=%v determined=%v, want %q %q 40 %v %v",
					tr.Profile, tr.PixelFormat, u16v(tr.Level), got, tr.ColourDetermined, tc.wantProfile, tc.wantPix, want, tc.wantDetermined)
			}
		})
	}
}

// TestVP9FromVpcC: a vpcC in CodecPrivate (how some muxers store it) gives the
// profile, level and bit depth head-only; a vpcC level of 0 falls back to the
// picture-size derivation.
func TestVP9FromVpcC(t *testing.T) {
	vpcC := func(level byte) []byte {
		return []byte{1, 0, 0, 0, 2, level, 10 << 4 /* 10-bit, 4:2:0, limited */, 9, 16, 9, 0, 0}
	}
	c, err := ReadMeta(context.Background(), bytes.NewReader(vp9MKV(vpcC(31), nil, 1920, 1080, nil)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.Profile != "Profile 2" || u16v(tr.Level) != 31 || u16v(tr.VideoBitDepth) != 10 || u16v(tr.ColorTransfer) != 16 {
		t.Errorf("vpcC: profile=%q level=%d depth=%d transfer=%d", tr.Profile, u16v(tr.Level), u16v(tr.VideoBitDepth), u16v(tr.ColorTransfer))
	}
	c, err = ReadMeta(context.Background(), bytes.NewReader(vp9MKV(vpcC(0), nil, 1920, 1080, nil)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if u16v(c.Tracks[0].Level) != 40 {
		t.Errorf("vpcC level 0: level=%d, want 40 derived", u16v(c.Tracks[0].Level))
	}
}

// TestVP9RealWebM pins the repo's encoder-produced WebM (no vpcC, as WebM is
// normally muxed): level head-only, profile and colour with the option. The
// encoder took an RGB source, so the keyframe declares profile 1, 4:4:4, sRGB
// (first block bytes a2 49 83 42 e0: marker 10, profile bits 1/0, key frame,
// sync code, color_space 111) - decoded by hand, not assumed.
func TestVP9RealWebM(t *testing.T) {
	data, err := os.ReadFile("../../internal/testdata/vp9.webm")
	if err != nil {
		t.Skip(err)
	}
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "vp9.webm", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	t.Logf("profile=%q level=%d depth=%d pix=%q prim/tx/mx/range=%d/%d/%d/%d determined=%v",
		tr.Profile, u16v(tr.Level), u16v(tr.VideoBitDepth), tr.PixelFormat,
		u16v(tr.ColorPrimaries), u16v(tr.ColorTransfer), u16v(tr.ColorSpace), u16v(tr.ColorRange), tr.ColourDetermined)
	if tr.Profile != "Profile 1" || u16v(tr.Level) != 10 || u16v(tr.VideoBitDepth) != 8 || tr.PixelFormat != "yuv444p" ||
		u16v(tr.ColorPrimaries) != 1 || u16v(tr.ColorTransfer) != 13 || u16v(tr.ColorSpace) != 0 || u16v(tr.ColorRange) != 2 {
		t.Errorf("real webm: profile=%q level=%d depth=%d pix=%q prim/tx/mx/range=%d/%d/%d/%d",
			tr.Profile, u16v(tr.Level), u16v(tr.VideoBitDepth), tr.PixelFormat,
			u16v(tr.ColorPrimaries), u16v(tr.ColorTransfer), u16v(tr.ColorSpace), u16v(tr.ColorRange))
	}
}

// TestVP9FeatureMetadata: the Matroska VP9 Codec Feature Metadata form of
// CodecPrivate (what mkvmerge writes - the bytes are a real file's) gives the
// profile and bit depth, a valid level when it carries one, and never a bogus
// level: without one the picture-size derivation stands.
func TestVP9FeatureMetadata(t *testing.T) {
	ctx := context.Background()
	realFile := []byte{0x01, 0x01, 0x00, 0x03, 0x01, 0x08, 0x04, 0x01, 0x01} // Profile 0, 8-bit, 4:2:0, no Level
	c, err := ReadMeta(ctx, bytes.NewReader(vp9MKV(realFile, nil, 1920, 1080, nil)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.Profile != "Profile 0" || u16v(tr.Level) != 40 || u16v(tr.VideoBitDepth) != 8 || tr.PixelFormat != "yuv420p" || tr.ColourDetermined || tr.ColorTransfer != nil {
		t.Errorf("feature metadata: profile=%q level=%d depth=%d pix=%q determined=%v transfer=%v, want Profile 0 / 40 derived / 8 / yuv420p / false / nil",
			tr.Profile, u16v(tr.Level), u16v(tr.VideoBitDepth), tr.PixelFormat, tr.ColourDetermined, tr.ColorTransfer)
	}
	// With a Level record the declared level wins over the derivation.
	withLevel := append(append([]byte{}, realFile...), 0x02, 0x01, 0x1F) // Level 3.1
	c, err = ReadMeta(ctx, bytes.NewReader(vp9MKV(withLevel, nil, 1920, 1080, nil)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if u16v(c.Tracks[0].Level) != 31 {
		t.Errorf("declared level: %d, want 31", u16v(c.Tracks[0].Level))
	}
	// A level outside the table is not a level.
	bogus := append(append([]byte{}, realFile...), 0x02, 0x01, 0x01)
	c, err = ReadMeta(ctx, bytes.NewReader(vp9MKV(bogus, nil, 1920, 1080, nil)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if u16v(c.Tracks[0].Level) != 40 {
		t.Errorf("bogus level 1: %d, want 40 derived", u16v(c.Tracks[0].Level))
	}
	// A vpcC (FullBox) declaring a bogus level falls back the same way.
	vpcCBogus := []byte{1, 0, 0, 0, 0, 99, 8 << 4, 1, 1, 1, 0, 0}
	c, err = ReadMeta(ctx, bytes.NewReader(vp9MKV(vpcCBogus, nil, 1920, 1080, nil)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if u16v(c.Tracks[0].Level) != 40 || c.Tracks[0].Profile != "Profile 0" {
		t.Errorf("vpcC bogus level: level=%d profile=%q, want 40 / Profile 0", u16v(c.Tracks[0].Level), c.Tracks[0].Profile)
	}
}

func TestParseVP9FeatureMetadataStrict(t *testing.T) {
	for _, bad := range [][]byte{
		nil,
		{1, 0, 0, 0, 0, 10, 8 << 4, 1, 1, 1, 0, 0}, // FullBox vpcC: second byte 0
		{0, 10, 8 << 4, 1, 1, 1, 0, 0},             // bare vpcC: second byte a level code
		{1, 1},                                     // truncated record
		{5, 1, 0},                                  // unknown id
		{1, 2, 0, 0},                               // length other than 1
	} {
		if _, ok := ParseVP9FeatureMetadata(bad); ok {
			t.Errorf("ParseVP9FeatureMetadata(% x) = ok, want rejected", bad)
		}
	}
}
