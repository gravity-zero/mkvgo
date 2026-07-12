package mkvgo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

var (
	testAVCC = []byte{0x01, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x42, 0x00, 0x1F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}
	testASC  = []byte{0x12, 0x10} // AAC-LC, 44100, stereo
)

// routerFixtureMKV builds an MKV whose audio (track 2) starts 300ms after
// the video - the defect RetimeTracks cancels.
func routerFixtureMKV(t *testing.T, dir string) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: testAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: testASC, SampleRate: &sr, Channels: &ch},
	}
	path := filepath.Join(dir, "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "test", WritingApp: "test"}}
	if err := mw.WriteMetadata(c, tracks, 4000); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		blocks := []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0xAA, byte(i)}},
		}
		if err := mw.WriteClusterWithCues(ts, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRetimeTracksRoutesByContent: the router picks the engine from the
// FIRST BYTES, not the extension - the MP4 fixture deliberately wears a
// .mkv name.
func TestRetimeTracksRoutesByContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	shift := map[uint64]int64{2: -300_000_000}

	mkvPath := routerFixtureMKV(t, dir)
	mp4Path := filepath.Join(dir, "mislabeled.mkv") // MP4 bytes, MKV name
	if err := mp4.RemuxToMP4(ctx, mkvPath, mp4Path); err != nil {
		t.Fatal(err)
	}

	if err := RetimeTracks(ctx, mkvPath, shift); err != nil {
		t.Fatalf("Matroska route: %v", err)
	}
	if err := RetimeTracks(ctx, mp4Path, shift); err != nil {
		t.Fatalf("MP4 route (mislabeled name): %v", err)
	}

	// Both repaired files still parse with their own reader.
	if _, err := matroska.OpenMeta(ctx, mkvPath); err != nil {
		t.Errorf("repaired MKV does not parse: %v", err)
	}
	if c, _, err := mp4.OpenMeta(ctx, mp4Path); err != nil || len(c.Tracks) != 2 {
		t.Errorf("repaired MP4 does not parse: %v", err)
	}
}

// TestRetimeTracksRefusesMatroskaOnlyOptionsOnMP4: engine-specific options
// must refuse loudly on the MP4 route, never drop silently.
func TestRetimeTracksRefusesMatroskaOnlyOptionsOnMP4(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "in.mp4")
	if err := mp4.RemuxToMP4(ctx, routerFixtureMKV(t, dir), mp4Path); err != nil {
		t.Fatal(err)
	}
	err := RetimeTracks(ctx, mp4Path, map[uint64]int64{2: -300_000_000}, matroska.Options{DeepVerify: true})
	if err == nil || !strings.Contains(err.Error(), "Matroska engine") {
		t.Errorf("Matroska-only options on MP4 must refuse with guidance, got %v", err)
	}
	// The same options ride fine on the Matroska route.
	if err := RetimeTracks(ctx, routerFixtureMKV(t, t.TempDir()), map[uint64]int64{2: -300_000_000}, matroska.Options{DeepVerify: true}); err != nil {
		t.Errorf("DeepVerify on the Matroska route must work: %v", err)
	}
}

// TestRetimeTracksUnknownContainer: garbage refuses with the sniffed reason.
func TestRetimeTracksUnknownContainer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("this is not a media file"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RetimeTracks(context.Background(), path, map[uint64]int64{1: 100_000_000})
	if err == nil || !strings.Contains(err.Error(), "unrecognised container") {
		t.Errorf("unknown container must refuse, got %v", err)
	}
}
