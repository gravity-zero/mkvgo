package mkvgo

import (
	"context"
	"errors"
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

// TestDiagnoseRoutesByContent: one Diagnose call covers both containers with
// the same report shape - the audio-delay finding carries the same retime
// remedy either way.
func TestDiagnoseRoutesByContent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mkvPath := routerFixtureMKV(t, dir) // audio +300ms
	mp4Path := filepath.Join(dir, "mislabeled.mkv")
	if err := mp4.RemuxToMP4(ctx, mkvPath, mp4Path); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{mkvPath, mp4Path} {
		d, err := Diagnose(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var found bool
		for _, f := range d.Findings {
			if f.Kind == "audio-delay" && f.Track == 2 && strings.Contains(f.Remedy, "retime --shift 2=-300") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want the audio-delay finding with the retime remedy, got %+v", path, d.Findings)
		}
		if d.AudioDelaysNs[2] != 300_000_000 {
			t.Errorf("%s: delay = %d, want 300ms", path, d.AudioDelaysNs[2])
		}
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

// TestSniffNamesMPEGTS: a transport stream (0x47 sync every 188 bytes) or an M2TS (every 192 after a timecode) is
// refused as MPEG-TS whatever its name, by the dispatch and by the Matroska reader alike, one error for both
// (matroska.ErrMPEGTS is mp4.ErrMPEGTS), not lumped into "unrecognised" or an EBML parse error.
func TestSniffNamesMPEGTS(t *testing.T) {
	packets := func(prefix, stride int) []byte {
		b := make([]byte, 20*stride)
		for i := range 20 {
			b[i*stride+prefix] = 0x47
		}
		return b
	}
	for _, c := range []struct {
		name string
		data []byte
	}{{"ts", packets(0, 188)}, {"m2ts", packets(4, 192)}} {
		for _, ext := range []string{".mp4", ".mkv", ".ts"} {
			path := filepath.Join(t.TempDir(), c.name+ext)
			if err := os.WriteFile(path, c.data, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Diagnose(context.Background(), path)
			if !errors.Is(err, mp4.ErrMPEGTS) || !errors.Is(err, matroska.ErrMPEGTS) || !strings.Contains(err.Error(), "MPEG-TS detected") {
				t.Errorf("Diagnose %s%s: %v, want the MPEG-TS refusal", c.name, ext, err)
			}
			if _, err := matroska.Open(context.Background(), path); !errors.Is(err, matroska.ErrMPEGTS) {
				t.Errorf("matroska.Open %s%s: %v, want matroska.ErrMPEGTS", c.name, ext, err)
			}
		}
	}

	// A lone 0x47 with no second sync stays "unrecognised": one byte is not a
	// transport stream, and mislabelling arbitrary data as MPEG-TS would be
	// worse than the generic message.
	lone := make([]byte, 200)
	lone[0] = 0x47
	lonePath := filepath.Join(t.TempDir(), "lone.bin")
	if err := os.WriteFile(lonePath, lone, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Diagnose(context.Background(), lonePath)
	if err == nil || !strings.Contains(err.Error(), "unrecognised") {
		t.Errorf("a lone 0x47 must stay unrecognised, got %v", err)
	}
}

// TestSniffRoutesAMOVOpeningOnPnot: a QuickTime movie whose first box is a preview (pnot) routes to the MP4 engine.
func TestSniffRoutesAMOVOpeningOnPnot(t *testing.T) {
	mov, err := os.ReadFile(filepath.Join("internal", "testdata", "bframes_aac.mov"))
	if err != nil {
		t.Fatal(err)
	}
	pnot := append([]byte{0, 0, 0, 20}, append([]byte("pnot"), make([]byte, 12)...)...)
	path := filepath.Join(t.TempDir(), "preview.mov")
	if err := os.WriteFile(path, append(pnot, mov...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "x" }); err != nil {
		t.Fatalf("a MOV opening on pnot: %v", err)
	}
	c, _, err := mp4.OpenMeta(context.Background(), path)
	if err != nil || c.Info.Title != "x" {
		t.Errorf("title %v, err %v", c, err)
	}
}
