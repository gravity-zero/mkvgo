package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/splitplan"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestSplitTitlesStayInTheOutputDir: chapter titles are file data; a part named after one stays under OutputDir
// (checked with filepath.Rel) and never takes a name Windows reserves for a device.
func TestSplitTitlesStayInTheOutputDir(t *testing.T) {
	titles := []string{"../../x", "a/b", `..\x`, "", "ctl\x01\x7f", "CON", "nul.txt", "Com1"}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}
	for i, title := range titles {
		c.Chapters = append(c.Chapters, mkv.Chapter{ID: uint64(i + 1), Title: title, StartMs: int64(i) * 1000})
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(c, []mkv.Track{videoTrack(1)}, int64(len(titles))*1000))
	for tc := int64(0); tc < int64(len(titles))*1000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0, Data: []byte("v")}}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())

	out := filepath.Join(dir, "out")
	parts, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: out, ByChapters: true, Pattern: "{title}.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != len(titles) {
		t.Fatalf("%d parts for %d chapters: %v", len(parts), len(titles), parts)
	}
	for i, p := range parts {
		rel, err := filepath.Rel(out, p)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.Dir(rel) != "." {
			t.Errorf("chapter %q: part %s is not directly under %s", titles[i], p, out)
		}
		base := strings.ToUpper(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			base = base[:dot]
		}
		if reservedOnWindows[base] {
			t.Errorf("chapter %q: part %s takes a name Windows reserves", titles[i], filepath.Base(p))
		}
	}
}

// reservedOnWindows are the device names Windows reserves, with or without an extension.
var reservedOnWindows = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// TestDeviceNamesWindowsReserves: Windows reserves COM0 and LPT0 and the superscript-digit forms COM¹ to LPT³ beside
// CON, PRN, AUX, NUL, COM1-9 and LPT1-9.
func TestDeviceNamesWindowsReserves(t *testing.T) {
	for _, s := range []string{"CON", "NUL", "COM0", "COM9", "LPT0", "COM¹", "COM²", "LPT³"} {
		if !splitplan.DeviceName(s) {
			t.Errorf("DeviceName(%q) = false, want reserved", s)
		}
	}
	for _, s := range []string{"COM", "COM10", "LPT⁴", "CONSOLE", "COMA"} {
		if splitplan.DeviceName(s) {
			t.Errorf("DeviceName(%q) = true, want free", s)
		}
	}
}
