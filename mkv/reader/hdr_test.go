package reader

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestHDRStaticMatroska covers reading HDR10 static metadata from a Matroska
// Colour element: MaxCLL/MaxFALL plus the MasteringMetadata (SMPTE ST 2086).
func TestHDRStaticMatroska(t *testing.T) {
	mastering := masterElem(mkv.IDMasteringMetadata,
		floatElem(mkv.IDPrimaryRChromaX, 0.708), floatElem(mkv.IDPrimaryRChromaY, 0.292),
		floatElem(mkv.IDPrimaryGChromaX, 0.170), floatElem(mkv.IDPrimaryGChromaY, 0.797),
		floatElem(mkv.IDPrimaryBChromaX, 0.131), floatElem(mkv.IDPrimaryBChromaY, 0.046),
		floatElem(mkv.IDWhitePointChromaX, 0.3127), floatElem(mkv.IDWhitePointChromaY, 0.3290),
		floatElem(mkv.IDLuminanceMax, 1000.0), floatElem(mkv.IDLuminanceMin, 0.005),
	)
	colour := masterElem(mkv.IDColour,
		uintElem(mkv.IDColourTransfer, 16, 1), // PQ
		uintElem(mkv.IDColourMaxCLL, 1000, 2),
		uintElem(mkv.IDColourMaxFALL, 400, 2),
		mastering,
	)
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDPixelWidth, 3840, 2), uintElem(mkv.IDPixelHeight, 2160, 2), colour)
	track := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		video,
	)
	file := segmentMKV(infoElem(), masterElem(mkv.IDTracks, track), clusterElem())

	c, err := Read(context.Background(), bytes.NewReader(file), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	hdr := c.Tracks[0].HDR
	if hdr == nil {
		t.Fatal("HDR is nil, want populated")
	}
	if hdr.MaxCLL != 1000 || hdr.MaxFALL != 400 {
		t.Errorf("MaxCLL/MaxFALL = %d/%d, want 1000/400", hdr.MaxCLL, hdr.MaxFALL)
	}
	md := hdr.MasteringDisplay
	if md == nil {
		t.Fatal("MasteringDisplay is nil")
	}
	for _, tc := range []struct {
		name      string
		got, want float64
	}{
		{"RedX", md.RedX, 0.708}, {"RedY", md.RedY, 0.292},
		{"GreenX", md.GreenX, 0.170}, {"GreenY", md.GreenY, 0.797},
		{"BlueX", md.BlueX, 0.131}, {"BlueY", md.BlueY, 0.046},
		{"WhiteX", md.WhiteX, 0.3127}, {"WhiteY", md.WhiteY, 0.3290},
		{"LumMax", md.LuminanceMax, 1000.0}, {"LumMin", md.LuminanceMin, 0.005},
	} {
		if math.Abs(tc.got-tc.want) > 1e-9 {
			t.Errorf("%s = %g, want %g", tc.name, tc.got, tc.want)
		}
	}
}
