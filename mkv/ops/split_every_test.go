package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func TestRangesEvery(t *testing.T) {
	// Keyframes every 2s over 10s; segments of ~5s → boundaries at 6000 (first
	// KF ≥ 5000) then EOF.
	keyframes := []int64{0, 2000, 4000, 6000, 8000, 10000}
	ranges, err := rangesEvery(keyframes, 5000)
	if err != nil {
		t.Fatal(err)
	}
	want := []mkv.TimeRange{{StartMs: 0, EndMs: 6000}, {StartMs: 6000, EndMs: 0}}
	if len(ranges) != len(want) || ranges[0] != want[0] || ranges[1] != want[1] {
		t.Errorf("ranges = %+v, want %+v", ranges, want)
	}

	if _, err := rangesEvery(nil, 5000); err == nil {
		t.Error("no keyframes: expected an explicit error")
	}
}

// Split with EveryMs produces keyframe-aligned segments; ByChapters with a
// {title} pattern names the parts after the sanitized chapter titles.
func TestSplit_EveryAndTitlePattern(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Intro", StartMs: 0, EndMs: 2000},
			{ID: 2, Title: "a/b: c?", StartMs: 2000, EndMs: 4000}, // needs sanitizing
			{ID: 3, Title: "Intro", StartMs: 4000},                // duplicate title
		},
	}
	tracks := []mkv.Track{videoTrack(1)}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(c, tracks, 6000))
	for tc := int64(0); tc < 6000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())

	// -every 3s → boundary at the first keyframe >= 3000 (4000), then EOF.
	parts, err := Split(context.Background(), mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "seg"), EveryMs: 3000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("EveryMs parts = %v, want 2", parts)
	}

	// Chapter split named by title, sanitized and de-duplicated.
	parts, err = Split(context.Background(), mkv.SplitOptions{
		SourcePath: src, OutputDir: filepath.Join(dir, "ch"),
		ByChapters: true, Pattern: "{title}.mkv",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Intro.mkv", "a_b_ c_.mkv", "Intro_3.mkv"}
	if len(parts) != len(want) {
		t.Fatalf("parts = %v, want %d", parts, len(want))
	}
	for i, w := range want {
		if filepath.Base(parts[i]) != w {
			t.Errorf("part[%d] = %s, want %s", i, filepath.Base(parts[i]), w)
		}
	}
}
