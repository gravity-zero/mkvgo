package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDolbyVisionCodecString: a Dolby Vision track whose base layer is not cross-compatible gets a Dolby sample
// entry (dvh1, dav1, dva1), and the CODECS / codecs string every manifest writes names it the same way -
// dvh1.PP.LL - where it said hvc1/av01/avc1, a codec the init does not carry.
func TestDolbyVisionCodecString(t *testing.T) {
	for _, tc := range []struct {
		codec string
		dv    mkv.DolbyVision
		want  string
	}{
		{"hevc", mkv.DolbyVision{Profile: 5, Level: 6}, "dvh1.05.06"},
		{"hevc", mkv.DolbyVision{Profile: 7, Level: 10}, "dvh1.07.10"},
		{"av1", mkv.DolbyVision{Profile: 10, Level: 9}, "dav1.10.09"},
		{"h264", mkv.DolbyVision{Profile: 9, Level: 5}, "dva1.09.05"},
	} {
		dv := tc.dv
		got := rfc6381Codec(&outTrack{mkv: mkv.Track{Codec: tc.codec, DolbyVision: &dv, CodecPrivate: make([]byte, 32)}})
		if got != tc.want {
			t.Errorf("%s DV profile %d level %d: codec string %q, want %q", tc.codec, tc.dv.Profile, tc.dv.Level, got, tc.want)
		}
	}
	// A cross-compatible profile (8.1: its base layer is HDR10) keeps the plain entry and the plain string.
	dv := mkv.DolbyVision{Profile: 8, Level: 6, BLSignalCompatID: 1}
	if got := rfc6381Codec(&outTrack{mkv: mkv.Track{Codec: "h264", DolbyVision: &dv, CodecPrivate: []byte{1, 0x64, 0, 0x1F}}}); got != "avc1.64001F" {
		t.Errorf("cross-compatible DV: codec string %q, want the plain avc1.64001F", got)
	}
}
