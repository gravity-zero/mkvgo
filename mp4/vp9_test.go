package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A VP9 profile-0 keyframe header: frame_marker=2, profile=0, keyframe,
// show_frame, sync code 0x498342, color_space=2 (BT.709), studio range.
var vp9Profile0Key = []byte{0x82, 0x49, 0x83, 0x42, 0x40, 0x00, 0x00, 0x00}

func TestParseVP9FrameHeader(t *testing.T) {
	h, err := parseVP9FrameHeader(vp9Profile0Key)
	if err != nil {
		t.Fatal(err)
	}
	if h.profile != 0 || h.bitDepth != 8 || h.chroma != 0 || h.fullRange {
		t.Errorf("profile-0 header = %+v, want profile 0, 8-bit, 4:2:0, studio range", h)
	}

	// Profile 2, ten_or_twelve_bit=0 → 10-bit, full range.
	p2 := []byte{0x92, 0x49, 0x83, 0x42, 0x18, 0x00}
	h, err = parseVP9FrameHeader(p2)
	if err != nil {
		t.Fatal(err)
	}
	if h.profile != 2 || h.bitDepth != 10 || !h.fullRange {
		t.Errorf("profile-2 header = %+v, want profile 2, 10-bit, full range", h)
	}

	for _, bad := range [][]byte{
		nil,
		{0x00},                   // bad frame_marker
		{0x86, 0x49, 0x83, 0x42}, // inter frame (frame_type=1)
		{0x82, 0x00, 0x00, 0x00}, // bad sync code
		{0x82, 0x49},             // truncated
	} {
		if _, err := parseVP9FrameHeader(bad); err == nil {
			t.Errorf("parseVP9FrameHeader(% x): expected error", bad)
		}
	}
}

// A Matroska VP9 track (no CodecPrivate, as mainstream muxers write them) remuxes to a
// vp09 sample entry whose vpcC is derived from the first keyframe, and comes
// back as vp9 through from-mp4's parser.
func TestVP9RoundTrip(t *testing.T) {
	w, h := uint32(320), uint32(240)
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h}},
		[]genBlock{
			{track: 1, pts: 0, key: true, data: vp9Profile0Key},
			{track: 1, pts: 40, key: false, data: []byte{0x86, 0x01}},
		})
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("vp09")) || !bytes.Contains(raw, []byte("vpcC")) {
		t.Fatal("MP4 lacks the vp09 entry or its vpcC config")
	}

	c, _, err := OpenMeta(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tracks) != 1 || c.Tracks[0].Codec != "vp9" {
		t.Fatalf("read-back tracks = %+v, want one vp9 track", c.Tracks)
	}
	tr := c.Tracks[0]
	if tr.Width == nil || *tr.Width != 320 || tr.Height == nil || *tr.Height != 240 {
		t.Errorf("dimensions = %vx%v, want 320x240", tr.Width, tr.Height)
	}
	// The MKV CodecPrivate is the Matroska VP9 Codec Feature Metadata built
	// from the vpcC (profile 0, level 2.0 for 320x240, 8-bit, 4:2:0) - not the
	// vpcC itself, which Matroska does not specify there - and the reader
	// turns it back into the same facts.
	wantCP := []byte{1, 1, 0, 2, 1, 20, 3, 1, 8, 4, 1, 0}
	if !bytes.Equal(tr.CodecPrivate, wantCP) {
		t.Errorf("CodecPrivate = % x, want the feature metadata % x", tr.CodecPrivate, wantCP)
	}
	if tr.Profile != "Profile 0" || tr.Level == nil || *tr.Level != 20 || tr.VideoBitDepth == nil || *tr.VideoBitDepth != 8 {
		t.Errorf("read-back: profile=%q level=%v depth=%v, want Profile 0 / 20 / 8", tr.Profile, tr.Level, tr.VideoBitDepth)
	}
}

// An existing VPCodecConfigurationRecord in CodecPrivate is used verbatim (no
// frame parsing).
func TestVP9EntryFromCodecPrivate(t *testing.T) {
	w, h := uint32(64), uint32(64)
	record := []byte{0, 10, 8 << 4, 1, 1, 1, 0, 0} // profile 0, level 1.0, 8-bit 4:2:0 studio, BT.709
	tr := mkv.Track{ID: 1, Codec: "vp9", Width: &w, Height: &h, CodecPrivate: record}
	entry, err := vp9Entry(&tr, nil) // no first frame needed
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entry, []byte("vpcC")) || !bytes.Contains(entry, record) {
		t.Errorf("entry must embed the CodecPrivate record verbatim")
	}
}

// A Matroska VP9 track whose CodecPrivate is the VP9 Codec Feature Metadata
// form (mkvmerge's) is NOT a vpcC: the record must come from the first
// keyframe, not from those bytes read as a record (which gave profile 1,
// level 1 - a codec string no player accepts).
func TestVP9FeatureMetadataIsNotAVpcC(t *testing.T) {
	w, h := uint32(320), uint32(240)
	feature := []byte{0x01, 0x01, 0x00, 0x03, 0x01, 0x08, 0x04, 0x01, 0x01}
	if got := vpcCRecord(feature); got != nil {
		t.Fatalf("vpcCRecord(feature metadata) = % x, want nil", got)
	}
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: &w, Height: &h, CodecPrivate: feature}},
		[]genBlock{{track: 1, pts: 0, key: true, data: vp9Profile0Key}})
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, out); err != nil {
		t.Fatal(err)
	}
	c, _, err := OpenMeta(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.Profile != "Profile 0" || tr.Level == nil || *tr.Level != 20 || tr.VideoBitDepth == nil || *tr.VideoBitDepth != 8 {
		t.Errorf("remuxed vpcC: profile=%q level=%v depth=%v, want Profile 0 / 20 (320x240) / 8", tr.Profile, tr.Level, tr.VideoBitDepth)
	}
}

// TestConfigRecordGuardAndInBandEntryTypes: a CodecPrivate that is not the
// configuration record its box would claim is refused with a message naming
// the record; a record without parameter sets gets the in-band entry type
// (avc3/hev1) and codec string.
func TestConfigRecordGuardAndInBandEntryTypes(t *testing.T) {
	w, h := uint32(320), uint32(240)
	blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88}}}
	// Annex-B parameter sets (start codes) where an avcC is expected.
	annexB := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x64, 0x00, 0x28}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: annexB, Width: &w, Height: &h}}, blocks)
	err := RemuxToMP4(context.Background(), src, filepath.Join(t.TempDir(), "o.mp4"))
	if err == nil || !strings.Contains(err.Error(), "not an AVCDecoderConfigurationRecord") {
		t.Fatalf("Annex-B CodecPrivate: err = %v, want the record guard", err)
	}
	// A bare sequence header OBU where an av1C is expected.
	obu := []byte{0x0a, 0x0b, 0x00, 0x00, 0x00, 0x42}
	src = buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: obu, Width: &w, Height: &h}}, blocks)
	err = RemuxToMP4(context.Background(), src, filepath.Join(t.TempDir(), "o.mp4"))
	if err == nil || !strings.Contains(err.Error(), "not an AV1CodecConfigurationRecord") {
		t.Fatalf("OBU CodecPrivate: err = %v, want the record guard", err)
	}

	// hvcC header from a real file with numOfArrays forced to 0: parameter
	// sets in-band → hev1.
	bare := mustHex(t, hevcHDRPrivateHex)[:23]
	bare[22] = 0
	tr := mkv.Track{ID: 1, Codec: "hevc", CodecPrivate: bare, Width: &w, Height: &h}
	entry, err := codecTable["hevc"].sampleEntry(&tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entry[:8], []byte("hev1")) {
		t.Errorf("bare hvcC entry type = %q, want hev1", entry[4:8])
	}
	if got := hevcCodecString(bare); !strings.HasPrefix(got, "hev1.") {
		t.Errorf("bare hvcC codec string = %q, want hev1.*", got)
	}
	full := mustHex(t, hevcHDRPrivateHex)
	if got := hevcCodecString(full); !strings.HasPrefix(got, "hvc1.") {
		t.Errorf("full hvcC codec string = %q, want hvc1.*", got)
	}
	// avcC with numOfSequenceParameterSets = 0 → avc3.
	avcBare := []byte{1, 100, 0, 40, 0xff, 0xe0, 0}
	tr = mkv.Track{ID: 1, Codec: "h264", CodecPrivate: avcBare, Width: &w, Height: &h}
	entry, err = codecTable["h264"].sampleEntry(&tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entry[:8], []byte("avc3")) {
		t.Errorf("bare avcC entry type = %q, want avc3", entry[4:8])
	}
	if got := rfc6381Codec(&outTrack{mkv: tr}); got != "avc3.640028" {
		t.Errorf("bare avcC codec string = %q, want avc3.640028", got)
	}
	tr.CodecPrivate = fakeAVCC
	if got := rfc6381Codec(&outTrack{mkv: tr}); !strings.HasPrefix(got, "avc1.") {
		t.Errorf("avcC with SPS codec string = %q, want avc1.*", got)
	}
}

// TestAACProfileThroughMP4: the AAC profile the AudioSpecificConfig states
// reaches the track on the MP4 probe too.
func TestAACProfileThroughMP4(t *testing.T) {
	ch := uint8(2)
	rate := 44100.0
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: &ch, SampleRate: &rate}},
		[]genBlock{{track: 1, pts: 0, key: true, data: []byte{0x21, 0x00}}})
	out := filepath.Join(t.TempDir(), "a.mp4")
	if err := RemuxToMP4(context.Background(), src, out); err != nil {
		t.Fatal(err)
	}
	c, _, err := OpenMeta(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tracks[0].Profile; got != "LC" {
		t.Errorf("MP4 AAC profile = %q, want LC", got)
	}
}
