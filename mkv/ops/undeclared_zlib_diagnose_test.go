package ops

import (
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// diagnose names a subtitle track whose cued block is an undeclared zlib stream, and only that one.
func TestDiagnoseFindsUndeclaredCompression(t *testing.T) {
	w, h := uint32(320), uint32(240)
	pcs := []byte{0x16, 0x00, 0x0B, 0x07, 0x80, 0x04, 0x38, 0x10, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x17, 0x00, 0x0A, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00}
	var blocks []mkv.Block
	for i := 0; i < 50; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: i%25 == 0, Data: append([]byte{0, 0, 1, 0x2D, 0x65}, make([]byte, 300)...)})
	}
	blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: 500, Keyframe: true, Data: zlibOf(pcs)}) // compressed, undeclared
	blocks = append(blocks, mkv.Block{TrackNumber: 3, Timecode: 500, Keyframe: true, Data: pcs})         // raw
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "pgs"},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "pgs"},
	}
	cues := []mkv.CuePoint{{TimeMs: 0, Track: 1}, {TimeMs: 500, Track: 2}, {TimeMs: 500, Track: 3}}
	src := buildMKVWithCues(t, t.TempDir(), "u.mkv", tracks, [][]mkv.Block{blocks[:25], blocks[25:]}, cues)
	d, err := Diagnose(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	for _, f := range d.Findings {
		if f.Kind == "undeclared-compression" {
			hits = append(hits, f.Detail)
		}
	}
	if len(hits) != 1 || !contains(hits[0], "track 2") {
		t.Errorf("findings: %v", hits)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
