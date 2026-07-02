package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// RemuxToHLS produces init.mp4 + segments + playlist; the init has mvex/trex and
// empty sample tables, each segment has styp/moof/mdat, and the playlist lists
// every segment. Boundaries fall on video keyframes.
func TestRemuxToHLS(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	// 6s of video (25fps, keyframe every second) + audio; ~2s segments → ~3 segs.
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		tc := int64(i) * 40
		gblocks = append(gblocks, genBlock{track: 1, pts: tc, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 300; i++ { // audio, 20ms frames
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
			data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		gblocks)

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}

	initData, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ftyp", "moov", "mvex", "trex", "iso5"} {
		if !bytes.Contains(initData, []byte(want)) {
			t.Errorf("init.mp4 missing %q box", want)
		}
	}
	// The init sample tables must be empty (no stco chunk offsets, no data).
	if bytes.Contains(initData, []byte("mdat")) {
		t.Error("init.mp4 must not contain media data")
	}

	pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#EXTM3U", "#EXT-X-MAP:URI=\"init.mp4\"", "#EXTINF:", "#EXT-X-ENDLIST"} {
		if !bytes.Contains(pl, []byte(want)) {
			t.Errorf("playlist missing %q", want)
		}
	}

	segs, _ := filepath.Glob(filepath.Join(dir, "seg*.m4s"))
	if len(segs) < 3 || len(segs) > 4 {
		t.Fatalf("segments = %d, want ~3 for 6s @ 2s target", len(segs))
	}
	seg1, err := os.ReadFile(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"styp", "moof", "mfhd", "traf", "tfhd", "tfdt", "trun", "mdat"} {
		if !bytes.Contains(seg1, []byte(want)) {
			t.Errorf("segment 1 missing %q box", want)
		}
	}

	// The temp files must be cleaned up.
	tmps, _ := filepath.Glob(filepath.Join(dir, ".mkvgo-hls-*.tmp"))
	if len(tmps) != 0 {
		t.Errorf("temp files left behind: %v", tmps)
	}
}

// A source with no video track is rejected (HLS needs a video rendition here).
func TestRemuxToHLS_AudioOnlyRejected(t *testing.T) {
	sr := 48000.0
	ch := uint8(2)
	var gblocks []genBlock
	for i := 0; i < 50; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch}},
		gblocks)
	if err := RemuxToHLS(context.Background(), src, t.TempDir()); err == nil {
		t.Fatal("audio-only source: expected an error (no video track)")
	}
}

func sortGenBlocks(b []genBlock) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && b[j].pts < b[j-1].pts; j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}
