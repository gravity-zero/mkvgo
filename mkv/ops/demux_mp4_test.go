package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

const demuxMP4Fixture = "../../internal/testdata/interleaved.mp4"

// TestDemuxMP4MatchesTheRemuxedMatroska: an MP4 demuxes to the same streams as the Matroska its remux writes, file
// for file: the same blocks go through the same formats (ADTS, Annex-B), so an MP4 needs no other tool.
func TestDemuxMP4MatchesTheRemuxedMatroska(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mkvPath := filepath.Join(dir, "src.mkv")
	mustNil(t, mp4.RemuxFromMP4(ctx, demuxMP4Fixture, mkvPath))
	for _, raw := range []bool{false, true} {
		fromMP4, fromMKV := filepath.Join(dir, "mp4"), filepath.Join(dir, "mkv")
		if raw {
			fromMP4, fromMKV = fromMP4+"-raw", fromMKV+"-raw"
		}
		mustNil(t, Demux(ctx, mkv.DemuxOptions{SourcePath: demuxMP4Fixture, OutputDir: fromMP4, Raw: raw}))
		mustNil(t, Demux(ctx, mkv.DemuxOptions{SourcePath: mkvPath, OutputDir: fromMKV, Raw: raw}))
		a, err := os.ReadDir(fromMP4)
		mustNil(t, err)
		b, err := os.ReadDir(fromMKV)
		mustNil(t, err)
		if len(a) != 2 || len(a) != len(b) {
			t.Fatalf("raw=%v: %d files from the MP4, %d from the Matroska, want 2 and 2", raw, len(a), len(b))
		}
		for i := range a {
			if a[i].Name() != b[i].Name() {
				t.Errorf("raw=%v: %s from the MP4, %s from the Matroska", raw, a[i].Name(), b[i].Name())
				continue
			}
			x, _ := os.ReadFile(filepath.Join(fromMP4, a[i].Name()))
			y, _ := os.ReadFile(filepath.Join(fromMKV, b[i].Name()))
			if len(x) == 0 || !bytes.Equal(x, y) {
				t.Errorf("raw=%v: %s differs (%d vs %d bytes)", raw, a[i].Name(), len(x), len(y))
			}
		}
	}
}

// TestDemuxMP4SelectsTracks: -t keeps the tracks named, by the IDs tracks lists, and refuses an unknown one.
func TestDemuxMP4SelectsTracks(t *testing.T) {
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "out")
	mustNil(t, Demux(ctx, mkv.DemuxOptions{SourcePath: demuxMP4Fixture, OutputDir: out, TrackIDs: []uint64{2}}))
	files, err := os.ReadDir(out)
	mustNil(t, err)
	if len(files) != 1 || files[0].Name() != "2.aac" {
		t.Errorf("files %v, want 2.aac alone", files)
	}
	if err := Demux(ctx, mkv.DemuxOptions{SourcePath: demuxMP4Fixture, OutputDir: out + "2", TrackIDs: []uint64{9}}); err == nil {
		t.Error("an unknown track ID was accepted")
	}
}
