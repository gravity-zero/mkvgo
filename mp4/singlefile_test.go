package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Single-file mode: one progressive file per rendition whose byte ranges are
// exactly the standalone segments (init+sidx head, then each fragment), the
// HLS playlist uses BYTERANGE, and the MPD the on-demand SegmentBase profile.
func TestSingleFileHLS(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 100; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 200; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}
	src := buildMKV(t, tracks, gblocks)

	segDir, sfDir := t.TempDir(), t.TempDir()
	if err := RemuxToHLS(context.Background(), src, segDir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(context.Background(), src, sfDir, Options{SegmentMs: 2000, SingleFile: true}); err != nil {
		t.Fatal(err)
	}

	// The single file = init + sidx + the exact segmented-mode fragments.
	stream, err := os.ReadFile(filepath.Join(sfDir, "stream.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	initSeg, _ := os.ReadFile(filepath.Join(segDir, "init.mp4"))
	if !bytes.HasPrefix(stream, initSeg) {
		t.Error("stream.mp4 must start with the init segment")
	}
	if !bytes.Contains(stream[:len(initSeg)+16], []byte("sidx")) {
		t.Error("sidx missing after the init")
	}
	seg1, _ := os.ReadFile(filepath.Join(segDir, "seg00001.m4s"))
	if !bytes.Contains(stream, seg1) {
		t.Error("fragment 1 not embedded verbatim")
	}

	// Playlist: MAP + per-segment byte ranges covering the file exactly.
	pl, _ := os.ReadFile(filepath.Join(sfDir, "playlist.m3u8"))
	for _, want := range []string{`#EXT-X-MAP:URI="stream.mp4",BYTERANGE="`, "#EXT-X-BYTERANGE:"} {
		if !bytes.Contains(pl, []byte(want)) {
			t.Errorf("playlist missing %q:\n%s", want, pl)
		}
	}
	if n := bytes.Count(pl, []byte("#EXT-X-BYTERANGE:")); n != bytes.Count(pl, []byte("#EXTINF:")) {
		t.Errorf("each segment needs a byte range (%d ranges)", n)
	}
	// No stray per-segment files in single-file mode.
	if m, _ := filepath.Glob(filepath.Join(sfDir, "seg*.m4s")); len(m) != 0 {
		t.Errorf("segment files written in single-file mode: %v", m)
	}
	if _, err := os.Stat(filepath.Join(sfDir, "stream_a1.mp4")); err != nil {
		t.Error("audio single file missing")
	}

	// MPD: on-demand profile with SegmentBase/indexRange.
	mpd, _ := os.ReadFile(filepath.Join(sfDir, "manifest.mpd"))
	for _, want := range []string{"isoff-on-demand", "<BaseURL>stream.mp4</BaseURL>", "SegmentBase indexRange=", `Initialization range="0-`} {
		if !bytes.Contains(mpd, []byte(want)) {
			t.Errorf("mpd missing %q:\n%s", want, mpd)
		}
	}

	// Encrypt + SingleFile is refused.
	if err := RemuxToHLS(context.Background(), src, t.TempDir(), Options{SingleFile: true,
		Encrypt: &HLSEncryption{Key: bytes.Repeat([]byte{1}, 16), KeyURI: "k"}}); err == nil {
		t.Error("SingleFile+Encrypt must be rejected")
	}
}
