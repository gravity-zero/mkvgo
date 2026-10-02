package mkv

import "testing"

// vfwPrivate builds the BITMAPINFOHEADER a V_MS/VFW/FOURCC track carries, with
// the FourCC in its biCompression field.
func vfwPrivate(fourCC string) []byte {
	p := make([]byte, 40)
	p[0] = 40
	copy(p[16:], fourCC)
	return p
}

// TestTrackFFprobeCodecName: the names a CodecID alone cannot give. A VFW
// track is whatever its FourCC says (the lookup by name returned "vfw" for an
// MPEG-4 track), a PCM track depends on its bit depth, and the CodecIDs with a
// "/" resolve here while FFprobeCodecName(string) leaves them alone. What
// cannot be resolved comes back as the raw CodecID, never as a guess.
func TestTrackFFprobeCodecName(t *testing.T) {
	depth := func(d uint8) *uint8 { return &d }
	for _, tc := range []struct {
		name  string
		track Track
		want  string
	}{
		{"VFW XVID", Track{Codec: "vfw", CodecPrivate: vfwPrivate("XVID")}, "mpeg4"},
		{"VFW lower case", Track{Codec: "vfw", CodecPrivate: vfwPrivate("div3")}, "msmpeg4v3"},
		{"VFW MJPG", Track{Codec: "vfw", CodecPrivate: vfwPrivate("MJPG")}, "mjpeg"},
		{"VFW unknown FourCC", Track{Codec: "vfw", CodecPrivate: vfwPrivate("ZZZZ")}, "V_MS/VFW/FOURCC"},
		{"VFW without a header", Track{Codec: "vfw"}, "V_MS/VFW/FOURCC"},
		{"PCM 8-bit", Track{Codec: "pcm", BitDepth: depth(8)}, "pcm_u8"},
		{"PCM 16-bit", Track{Codec: "pcm", BitDepth: depth(16)}, "pcm_s16le"},
		{"PCM 24-bit", Track{Codec: "pcm", BitDepth: depth(24)}, "pcm_s24le"},
		{"PCM no depth", Track{Codec: "pcm"}, "A_PCM/INT/LIT"},
		{"PCM big-endian 32-bit", Track{Codec: "A_PCM/INT/BIG", BitDepth: depth(32)}, "pcm_s32be"},
		{"PCM float 32", Track{Codec: "A_PCM/FLOAT/IEEE", BitDepth: depth(32)}, "pcm_f32le"},
		{"PCM float 64", Track{Codec: "A_PCM/FLOAT/IEEE", BitDepth: depth(64)}, "pcm_f64le"},
		{"MP3", Track{Codec: "A_MPEG/L3"}, "mp3"},
		{"MPEG-4 ASP", Track{Codec: "V_MPEG4/ISO/ASP"}, "mpeg4"},
		{"AAC profile ID", Track{Codec: "A_AAC/MPEG4/LC/SBR"}, "aac"},
		{"short name", Track{Codec: "srt"}, "subrip"},
		{"single-word ID", Track{Codec: "V_THEORA"}, "theora"},
		{"identical in both", Track{Codec: "h264"}, "h264"},
		{"unlisted ID", Track{Codec: "V_REAL/RV10"}, "V_REAL/RV10"},
	} {
		if got := tc.track.FFprobeCodecName(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// The lookup by name keeps its contract: a "/" is returned as it is.
	if got := FFprobeCodecName("A_MPEG/L3"); got != "A_MPEG/L3" {
		t.Errorf("FFprobeCodecName(%q) = %q, want it unchanged", "A_MPEG/L3", got)
	}
}
