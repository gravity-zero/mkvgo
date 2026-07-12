package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	// The vpcC became the MKV CodecPrivate (FullBox form) so bit depth/colour
	// survive; the reader's vp9Colour accepts it.
	if len(tr.CodecPrivate) < 12 {
		t.Errorf("CodecPrivate = % x, want the vpcC record (FullBox form)", tr.CodecPrivate)
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
