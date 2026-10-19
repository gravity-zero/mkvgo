package mp4

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// trakGroups returns each trak's handler and tkhd alternate_group, in file order.
func trakGroups(t *testing.T, data []byte) [][2]string {
	t.Helper()
	top, err := iterBoxes(data)
	if err != nil {
		t.Fatal(err)
	}
	moov, _ := findMemBox(top, "moov")
	kids, err := iterBoxes(moov.payload)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, k := range kids {
		if k.typ != "trak" {
			continue
		}
		tb, _ := iterBoxes(k.payload)
		tkhd, _ := findMemBox(tb, "tkhd")
		off := 34
		if tkhd.payload[0] == 1 {
			off = 46 // version 1: creation and modification times on 64 bits
		}
		group := binary.BigEndian.Uint16(tkhd.payload[off : off+2])
		out = append(out, [2]string{trakHandler(tb), string(rune('0' + group))})
	}
	return out
}

// TestRemuxGroupsAlternateTracksAsTheMainstreamMuxer pins that to-mp4 states which tracks are alternatives, as
// the mainstream muxer was measured to (video 0, audio 1, subtitles 3, whatever their count): with every group 0, Apple players
// showed one audio language and hid the other. The chapter track stays out of any group.
func TestRemuxGroupsAlternateTracksAsTheMainstreamMuxer(t *testing.T) {
	tracks := synthFixtureTracks()
	sr, ch := 44100.0, uint8(2)
	tracks = append(tracks,
		mkv.Track{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "fre"},
		mkv.Track{ID: 4, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"})
	blocks := synthFixtureBlocks(0)
	for _, b := range blocks {
		if b.track == 2 {
			blocks = append(blocks, genBlock{track: 3, pts: b.pts, key: true, data: b.data})
		}
	}
	blocks = append(blocks, genBlock{track: 4, pts: 0, key: true, data: []byte("bonjour")})
	sortGenBlocks(blocks)
	src := buildMKVWithChapters(t, tracks, blocks, []mkv.Chapter{{StartMs: 0, Title: "Un"}, {StartMs: 1000, Title: "Deux"}})
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	got := trakGroups(t, data)
	want := [][2]string{{"vide", "0"}, {"soun", "1"}, {"soun", "1"}, {"text", "3"}, {"text", "0"}}
	if len(got) != len(want) {
		t.Fatalf("traks %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("traks %v, want %v (video 0, audio 1, subtitles 3, the chapter track last in no group)", got, want)
			break
		}
	}
}
