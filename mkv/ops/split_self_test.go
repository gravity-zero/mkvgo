package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestSplitNeverWritesOverItsSource pins that a part named like the source is refused before any part is written:
// re-splitting part_001.mkv into its own folder truncated it, then removed it as an unfinished output.
func TestSplitNeverWritesOverItsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "part_001.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1)}, 6000))
	for tc := int64(0); tc < 6000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: tc%2000 == 0, Data: []byte("v")}}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: dir, EveryMs: 2000}); err == nil {
		t.Error("a split writing its first part over its source succeeded")
	}
	after, err := os.ReadFile(src)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the source was changed or removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "part_002.mkv")); !os.IsNotExist(err) {
		t.Error("a part was written before the refusal")
	}
}
