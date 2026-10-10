package mp4

import (
	"bytes"
	"compress/zlib"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func zlibBytes(raw []byte) []byte {
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	return zb.Bytes()
}

// compressedFixture is video plus one audio track whose payloads the track
// declares compressed with scheme c, and a zlib SRT and a zlib PGS subtitle.
func compressedFixture(t *testing.T, c mkv.Compression, raw []byte) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	audio := zlibBytes(raw)
	if c != mkv.CompressionZlib {
		audio = raw
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: audio})
	}
	for i := 0; i < 3; i++ {
		blocks = append(blocks, genBlock{track: 3, pts: int64(i) * 1000, key: true, data: zlibBytes([]byte("sous-titre " + string(rune('A'+i))))})
		blocks = append(blocks, genBlock{track: 4, pts: int64(i) * 1000, key: true, data: zlibBytes([]byte{0x80, 0x00, 0x00, 0x00})})
	}
	sortGenBlocks(blocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Compression: c},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre", Compression: mkv.CompressionZlib},
		{ID: 4, Type: mkv.SubtitleTrack, Codec: "pgs", Language: "fre", Compression: mkv.CompressionZlib},
	}, blocks)
}

// A zlib audio track reaches the player inflated, in the full pass and the
// plan alike; the zlib SRT rendition carries its text; the zlib PGS track is
// dropped as every PGS track is; the audio rendition is served from memory by
// Open while the video streams.
func TestCompressedTracksInflated(t *testing.T) {
	raw := bytes.Repeat([]byte{0x11, 0x22, 0x33}, 200)
	src := compressedFixture(t, mkv.CompressionZlib, raw)
	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.fts()) != 2 || len(plan.subs) != 1 {
		t.Fatalf("%d renditions, %d subtitle renditions: want video+audio and the SRT only", len(plan.fts()), len(plan.subs))
	}
	for _, name := range []string{"seg_a1_00001.m4s", "sub1.vtt", "seg00001.m4s"} {
		got, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: plan differs from the full pass", name)
		}
		switch name {
		case "seg_a1_00001.m4s":
			if !bytes.Contains(got, raw) || bytes.Contains(got, zlibBytes(raw)) {
				t.Errorf("audio segment must carry the inflated payload, not the zlib stream")
			}
		case "sub1.vtt":
			if !strings.Contains(string(got), "sous-titre A") {
				t.Errorf("the zlib SRT rendition lost its text:\n%s", got)
			}
		}
	}
	assertOpenMatchesResource(t, plan)
	if st := plan.Stats(); st.StreamedSegments == 0 {
		t.Errorf("the video must stream: %+v", st)
	}
}

// A scheme mkvgo cannot decode is refused, or dropped when asked to skip it.
func TestUndecodableCompressionRefused(t *testing.T) {
	src := compressedFixture(t, mkv.CompressionLZO1X, []byte{1, 2, 3, 4})
	ctx := context.Background()
	if _, err := PlanHLS(ctx, src, Options{SegmentMs: 2000}); err == nil || !strings.Contains(err.Error(), "ContentCompression") {
		t.Fatalf("PlanHLS must refuse the lzo track, got %v", err)
	}
	var dropped []DroppedTrack
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000, SkipUnsupported: true, OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.fts()) != 1 || len(dropped) < 1 || dropped[0].ID != 2 {
		t.Fatalf("with SkipUnsupported: %d renditions, dropped %+v", len(plan.fts()), dropped)
	}
}
