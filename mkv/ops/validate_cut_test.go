package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestValidateCuesPastTheEnd: on a file cut between two clusters, the cues
// naming clusters the file no longer holds are not a stale index a reindex
// would fix: they are reported as past the end, with the source as remedy.
func TestValidateCuesPastTheEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	whole := filepath.Join(dir, "headcues.mkv")
	headCuesMKV(t, whole)
	data := readAll(t, whole)
	// Seal the Segment's size (the fixture streams it): the cut is then measured against a declared end.
	seg := bytes.Index(data, []byte{0x18, 0x53, 0x80, 0x67})
	n := uint64(len(data) - seg - 12)
	copy(data[seg+4:], []byte{0x01, byte(n >> 48), byte(n >> 40), byte(n >> 32), byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	clusters := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if len(clusters) < 2 {
		t.Fatalf("fixture has %d clusters, want 2", len(clusters))
	}
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data[:clusters[1]])
	issues, err := Validate(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	past := false
	for _, i := range issues {
		if i.Code == "cues-stale" {
			t.Errorf("a cut file's missing clusters reported as a stale index: %s", i.Message)
		}
		past = past || i.Code == "cues-past-end"
	}
	if !past {
		t.Errorf("no cues-past-end issue: %+v", issues)
	}
}

// TestCueHealthOnACutFile: the index a cut file lost with its tail is not
// one a reindex restores first - the file is incomplete - and the verdict's
// reason says so rather than sending the operator to a reindex alone.
func TestCueHealthOnACutFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 8)
	for i := range 8 {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: make([]byte, 64)},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: make([]byte, 16)},
		})
	}
	whole := buildMultiClusterMKV(t, dir, "cued.mkv", tracks, sets, 8000)
	data := readAll(t, whole)
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data[:findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})[5]])
	for path, want := range map[string]bool{whole: false, cut: true} {
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(r.Reason, "cut short"); got != want {
			t.Errorf("%s: reason %q names the cut %v, want %v", filepath.Base(path), r.Reason, got, want)
		}
	}
}
