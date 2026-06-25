package reader

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestColourDetermined covers the colour-determinacy signal: a stream whose VUI
// is parsed reports ColourDetermined even when the colour resolves to
// "unspecified" (the confirmed-SDR case), while a stream whose colour cannot be
// read at all stays undetermined — letting a caller distinguish the two.
func TestColourDetermined(t *testing.T) {
	// AVC with a VUI carrying colour_description (present) but every code point
	// "unspecified" (2): determined, yet every Color* stays nil.
	sdr := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildHighSPSAvcC(2, 2, 2)}
	fillColourFromCodecPrivate(&sdr)
	if !sdr.ColourDetermined {
		t.Error("a parsed VUI with unspecified colour must be determined (confirmed SDR)")
	}
	if sdr.ColorPrimaries != nil || sdr.ColorTransfer != nil || sdr.ColorSpace != nil {
		t.Error("unspecified colour code points must stay nil")
	}

	// AVC with real colour (BT.709 matrix): determined, with a value.
	bt709 := mkv.Track{Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: buildHighSPSAvcC(2, 2, 1)}
	fillColourFromCodecPrivate(&bt709)
	if !bt709.ColourDetermined || bt709.ColorSpace == nil {
		t.Error("a VUI carrying BT.709 must be determined with a colour_space")
	}

	// A bare hvcC with no SPS arrays: the colour cannot be read → undetermined,
	// the case a caller should treat as "fall back".
	bareHvcC := make([]byte, 23)
	bareHvcC[0] = 1 // configurationVersion
	bareHvcC[1] = 2 // general_profile_idc = 2 (Main 10); byte 22 (numArrays) stays 0
	bare := mkv.Track{Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: bareHvcC}
	fillColourFromCodecPrivate(&bare)
	if bare.ColourDetermined {
		t.Error("a bare hvcC with no SPS must NOT be determined (colour unread)")
	}
}
