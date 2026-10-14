package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// negativeSttsMP4 is a video-only MP4 of 100 frames at 40 ms, one frame held
// 1 s, whose stts then has that 1 s entry rewritten as a negative delta
// stored unsigned (2^32 - 479970), the way some muxers write it.
func negativeSttsMP4(t *testing.T) []byte {
	t.Helper()
	w, h := uint32(320), uint32(240)
	var blocks []genBlock
	pts := int64(0)
	for i := 0; i < 100; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: pts, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
		pts += 40
		if i == 50 {
			pts += 960
		}
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	dst := filepath.Join(t.TempDir(), "v.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte("stts"))
	n := int(binary.BigEndian.Uint32(data[i+8:]))
	var max, at int
	for k := 0; k < n; k++ {
		if d := int(binary.BigEndian.Uint32(data[i+16+8*k:])); d > max {
			max, at = d, i+16+8*k
		}
	}
	if n < 3 || max == 0 {
		t.Fatalf("stts has %d entries, no held frame to rewrite", n)
	}
	binary.BigEndian.PutUint32(data[at:], 1<<32-479970)
	return data
}

// A negative stts delta stored unsigned is clipped to one tick, as reference
// decoders do: the timeline stays increasing, and the track's duration is its
// header's, not 2^32 ticks.
func TestNegativeSttsDeltaClipped(t *testing.T) {
	data := negativeSttsMP4(t)
	for _, mode := range []sampleMode{sampleNone, sampleKeyframes, sampleFull} {
		mv := parseFile(t, data, mode)
		c := containerFromMovie(mv)
		if want := mv.tracks[0].durationMs; c.DurationMs != want || want < 4000 || want > 6000 {
			t.Errorf("mode %d: duration %d ms, want the mdhd's %d ms", mode, c.DurationMs, want)
		}
		if mode == sampleFull {
			s := mv.tracks[0].samples
			for k := 1; k < len(s); k++ {
				if s[k].dtsMs < s[k-1].dtsMs || s[k].dtsMs > 10_000 {
					t.Fatalf("sample %d decodes at %d ms after %d ms", k, s[k].dtsMs, s[k-1].dtsMs)
				}
			}
		}
	}
}
