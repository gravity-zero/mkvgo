package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// sampleMP4 builds a small valid MP4 (via the MKV→MP4 path) and returns its bytes,
// for use as a fuzz seed and parser sanity input.
func sampleMP4(tb testing.TB) []byte {
	tb.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	var blocks []genBlock
	for ts := int64(0); ts < 120; ts += 40 {
		blocks = append(blocks, genBlock{track: 1, pts: ts, key: ts == 0, data: []byte{1, 2, 3, byte(ts)}})
		blocks = append(blocks, genBlock{track: 2, pts: ts, key: true, data: []byte{4, 5, byte(ts)}})
	}
	src := buildMKV(tb, tracks, blocks)
	dst := filepath.Join(tb.TempDir(), "seed.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err != nil {
		tb.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		tb.Fatal(err)
	}
	return data
}

// FuzzParseMP4 feeds arbitrary bytes to the MP4 parser. It must never panic;
// malformed input must surface as an error.
func FuzzParseMP4(f *testing.F) {
	f.Add([]byte{})
	f.Add(buildFtyp([]string{"avc1"}))
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, 'm', 'o', 'o', 'v'})
	f.Add([]byte{0x00, 0x00, 0xFF, 0xFF, 'm', 'o', 'o', 'v', 0x01, 0x02})
	f.Add(sampleMP4(f))

	f.Fuzz(func(t *testing.T, data []byte) {
		mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), true)
		if err == nil && mv == nil {
			t.Fatal("nil movie with nil error")
		}
		// iterBoxes is reachable on the moov payload; exercise it directly too.
		_, _ = iterBoxes(data)
	})
}

// FuzzDescriptors fuzzes the MPEG-4 descriptor and codec-config sub-parsers,
// which run on attacker-controlled CodecPrivate-derived bytes.
func FuzzDescriptors(f *testing.F) {
	f.Add([]byte{0x03, 0x05, 0x00, 0x00, 0x00, 0x04, 0x00})
	f.Add(esdsBox(0x40, []byte{0x12, 0x10})[8:])

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = parseESDS(data)
		_, _ = opusHeadFromDOps(data)
		_, _ = dOpsBox(data)
		_, _ = parseAC3(data)
		_, _ = parseEAC3(data)
	})
}
