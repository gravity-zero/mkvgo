package commands_test

import (
	"encoding/json"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// pgsFixtureMKV writes a source with one PGS subtitle track holding two cues:
// a display set at 0 ms and the empty composition that clears it at 500 ms.
func pgsFixtureMKV(t *testing.T) string {
	t.Helper()
	seg := func(typ byte, payload []byte) []byte {
		return append([]byte{typ, byte(len(payload) >> 8), byte(len(payload))}, payload...)
	}
	rle := []byte{0x00, 0x84, 0x01, 0x00, 0x00, 0x00, 0x00} // 4 white pixels, then an empty line
	show := seg(0x16, []byte{                               // 1920x1080, epoch start, one object at (100,900)
		0x07, 0x80, 0x04, 0x38, 0x10, 0x00, 0x00, 0x80, 0x00, 0x00, 0x01,
		0x00, 0x0a, 0x00, 0x40, 0x00, 0x64, 0x03, 0x84, // forced
	})
	show = append(show, seg(0x14, []byte{0x00, 0x00, 0x01, 235, 128, 128, 255})...)
	show = append(show, seg(0x15, append([]byte{0x00, 0x0a, 0x00, 0xc0,
		0x00, 0x00, byte(len(rle) + 4), 0x00, 0x04, 0x00, 0x02}, rle...))...)
	show = append(show, seg(0x80, nil)...)
	clear := append(seg(0x16, []byte{0x07, 0x80, 0x04, 0x38, 0x10, 0, 0, 0, 0, 0, 0}), seg(0x80, nil)...)

	path := filepath.Join(t.TempDir(), "pgs.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"}}
	tracks := []mkv.Track{{ID: 1, Type: mkv.SubtitleTrack, Codec: "pgs", Language: "eng"}}
	if err := mw.WriteMetadata(c, tracks, 1000); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteClusterWithCues(0, 1000000, []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Data: show},
		{TrackNumber: 1, Timecode: 500, Data: clear},
	}); err != nil {
		t.Fatal(err)
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The CLI's PGS output is the on-disk form of a PGSCue: one PNG per cue plus a
// manifest that carries the timing and the screen position the picture belongs
// at - the picture alone says nothing about where to draw it.
func TestCmdExtractSubtitle_PGSWritesPNGsAndManifest(t *testing.T) {
	src := pgsFixtureMKV(t)
	outDir := filepath.Join(t.TempDir(), "subs")

	out := capture(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "1", "-o", outDir, "-format", "pgs"})
	})
	if !strings.Contains(out, "extracted 1 PGS cue(s)") {
		t.Errorf("stdout = %q, want the cue count", out)
	}

	blob, err := os.ReadFile(filepath.Join(outDir, "cues.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest []struct {
		StartMs int64  `json:"start_ms"`
		EndMs   int64  `json:"end_ms"`
		X       int    `json:"x"`
		Y       int    `json:"y"`
		ScreenW int    `json:"screen_w"`
		ScreenH int    `json:"screen_h"`
		Forced  bool   `json:"forced"`
		File    string `json:"file"`
	}
	if err := json.Unmarshal(blob, &manifest); err != nil {
		t.Fatalf("cues.json: %v", err)
	}
	if len(manifest) != 1 {
		t.Fatalf("manifest holds %d cue(s), want 1", len(manifest))
	}
	m := manifest[0]
	if m.StartMs != 0 || m.EndMs != 500 {
		t.Errorf("cue spans %d..%d ms, want 0..500", m.StartMs, m.EndMs)
	}
	if m.X != 100 || m.Y != 900 || m.ScreenW != 1920 || m.ScreenH != 1080 {
		t.Errorf("cue at (%d,%d) on %dx%d, want (100,900) on 1920x1080", m.X, m.Y, m.ScreenW, m.ScreenH)
	}
	if !m.Forced {
		t.Error("the forced flag did not reach the manifest")
	}

	f, err := os.Open(filepath.Join(outDir, m.File))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding the written PNG: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 4 || h != 2 {
		t.Errorf("PNG is %dx%d, want 4x2", w, h)
	}
	r, g, b, a := img.At(0, 0).RGBA()
	if r>>8 != 255 || g>>8 != 255 || b>>8 != 255 || a>>8 != 255 {
		t.Errorf("top-left pixel = %d,%d,%d,%d, want opaque white", r>>8, g>>8, b>>8, a>>8)
	}
	if _, _, _, a := img.At(0, 1).RGBA(); a != 0 {
		t.Errorf("an unpainted pixel has alpha %d, want 0 - the straight alpha was lost", a)
	}
}

// -index must serve PGS too: that is the whole point of the index for a caller
// that already caches one.
func TestCmdExtractSubtitle_PGSFromIndex(t *testing.T) {
	src := pgsFixtureMKV(t)
	dir := t.TempDir()
	ixPath := filepath.Join(dir, "subs.mkvsix")
	capture(t, func() {
		commands.CmdSubtitleIndex([]string{src, "-o", ixPath})
	})

	outDir := filepath.Join(dir, "out")
	out := capture(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "1", "-o", outDir, "-format", "pgs", "-index", ixPath})
	})
	if !strings.Contains(out, "extracted 1 PGS cue(s)") {
		t.Errorf("stdout = %q, want the cue count", out)
	}
	if _, err := os.Stat(filepath.Join(outDir, "00000.png")); err != nil {
		t.Errorf("no picture written: %v", err)
	}
}

// A text track asked for as PGS must say what it actually is, and point at the
// command that works - the message is what the operator sees.
func TestCmdExtractSubtitle_PGSOnTextTrackFatal(t *testing.T) {
	src := writeMKV(t, richContainer())
	out := filepath.Join(t.TempDir(), "subs")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "3", "-o", out, "-format", "pgs"})
	})
}

// -index still refuses the formats that cannot use one.
func TestCmdExtractSubtitle_PGSIndexRejectsSRT(t *testing.T) {
	src := pgsFixtureMKV(t)
	dir := t.TempDir()
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "1", "-o", filepath.Join(dir, "o.srt"),
			"-format", "srt", "-index", filepath.Join(dir, "nope.mkvsix")})
	})
}

var _ = matroska.PGSCue{} // the facade type the CLI is the parity partner of
