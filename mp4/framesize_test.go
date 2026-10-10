package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func audioTrack(codec string, rate float64, private []byte) *mkv.Track {
	return &mkv.Track{Type: mkv.AudioTrack, Codec: codec, SampleRate: &rate, CodecPrivate: private}
}

// eac3Frame is a 64-byte E-AC-3 syncframe header holding blocks six-block groups at 48 kHz.
func eac3SyncFrame(numblkscod byte) []byte {
	b := make([]byte, 64)
	b[0], b[1] = 0x0B, 0x77
	b[2], b[3] = 0x00, 0x1F // strmtyp 0, substream 0, frmsiz 31 -> 64 bytes
	b[4] = 0<<6 | numblkscod<<4 | 2<<1
	b[5] = 16 << 3 // bsid 16
	return b
}

// The frame size comes from the stream: the AAC config's frame length and
// SBR rate, the E-AC-3 syncframe's block count, the DTS core header, the MP3
// header's version, a fixed FLAC block size, TrueHD by specification.
func TestFrameSamplesFromStream(t *testing.T) {
	heaac := []byte{0x2B, 0x11, 0x88} // AOT 5 (SBR), core 24 kHz, stereo, output 48 kHz, core AOT 2
	for _, tc := range []struct {
		name string
		t    *mkv.Track
		want int64
	}{
		{"AAC-LC 1024", audioTrack("aac", 48000, []byte{0x11, 0x90}), 1024},
		{"AAC 960", audioTrack("aac", 48000, []byte{0x11, 0x94}), 960},
		{"HE-AAC at the core rate", audioTrack("aac", 24000, heaac), 1024},
		{"HE-AAC at the output rate", audioTrack("aac", 48000, heaac), 2048},
		{"MP3 MPEG-1", audioTrack("mp3", 44100, nil), 1152},
		{"MP3 MPEG-2 LSF", audioTrack("mp3", 24000, nil), 576},
		{"TrueHD 48k", audioTrack("truehd", 48000, nil), 40},
		{"TrueHD 96k", audioTrack("truehd", 96000, nil), 80},
		{"FLAC fixed 4096", audioTrack("flac", 48000, append([]byte("fLaC"), 0x80, 0, 0, 34, 0x10, 0x00, 0x10, 0x00, 0, 0, 0, 0)), 4096},
		{"FLAC variable", audioTrack("flac", 48000, append([]byte("fLaC"), 0x80, 0, 0, 34, 0x00, 0x10, 0x10, 0x00, 0, 0, 0, 0)), 0},
		{"E-AC-3 header alone says nothing", audioTrack("eac3", 48000, nil), 0},
	} {
		if got := frameSamplesFromHeader(tc.t); got != tc.want {
			t.Errorf("%s: header frame samples %d, want %d", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name  string
		codec string
		data  []byte
		want  int64
	}{
		{"E-AC-3 one block", "eac3", eac3SyncFrame(0), 256},
		{"E-AC-3 two blocks", "eac3", eac3SyncFrame(1), 512},
		{"E-AC-3 three blocks", "eac3", eac3SyncFrame(2), 768},
		{"E-AC-3 six blocks", "eac3", eac3SyncFrame(3), 1536},
		{"DTS core 16 blocks", "dts", []byte{0x7F, 0xFE, 0x80, 0x01, 0xFC, 0x3C, 0x00, 0x00}, 512},
		{"MP3 MPEG-2 layer III", "mp3", []byte{0xFF, 0xF3, 0x90, 0x00}, 576},
		{"MP3 MPEG-1 layer III", "mp3", []byte{0xFF, 0xFB, 0x90, 0x00}, 1152},
		{"garbage", "dts", []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0},
	} {
		if got := frameSamplesFromPayload(tc.codec, tc.data); got != tc.want {
			t.Errorf("%s: payload frame samples %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A measured stride yields to the frame the stream states whatever the
// millisecond rounding or a gap did to the measurement, snaps to a known codec
// size within a percent otherwise, and stays as measured when neither applies.
func TestSnapGridTSToCodecFrame(t *testing.T) {
	rate := 48000.0
	mk := func(codec string, frame int64) *outTrack {
		return &outTrack{mkv: mkv.Track{Codec: codec, SampleRate: &rate}, frameSamples: frame}
	}
	for _, tc := range []struct {
		name     string
		t        *outTrack
		measured int64
		want     int64
	}{
		{"AAC laced 171 ms / 8", mk("aac", 1024), 1026, 1024},
		{"AAC header 960 from a 170 ms block", mk("aac", 960), 1020, 960},
		{"E-AC-3 256-sample frames, 43 ms / 8", mk("eac3", 256), 258, 256},
		{"E-AC-3 512-sample frames, 85 ms / 8", mk("eac3", 512), 510, 512},
		{"E-AC-3 768-sample frames, exact", mk("eac3", 768), 768, 768},
		{"DTS 512, 86 ms / 8", mk("dts", 512), 516, 512},
		{"DTS unknown header, 86 ms / 8 by the codec list", mk("dts", 0), 516, 512},
		{"header stands against a gap between the first blocks", mk("aac", 1024), 1500, 1024},
		{"TrueHD frames under the timecode resolution", mk("truehd", 40), 24, 40},
		{"codec list only, AAC", mk("aac", 0), 1026, 1024},
		{"MP3 LSF by the list", mk("mp3", 0), 574, 576},
		{"no size known", mk("opus", 0), 1026, 1026},
	} {
		if got := snapGridTS(tc.measured, tc.t, 48000); got != tc.want {
			t.Errorf("%s: %d snapped to %d, want %d", tc.name, tc.measured, got, tc.want)
		}
	}
}
