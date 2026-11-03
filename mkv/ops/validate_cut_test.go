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

// corruptCues rewrites, inside the Cues element of data, every CueClusterPosition to its width's largest value (a
// position past the end of the file) and, with retrack, the first CueTrack to track 2: a corrupt index.
func corruptCues(t *testing.T, data []byte, retrack bool) []byte {
	t.Helper()
	out := append([]byte{}, data...)
	start := bytes.Index(out, []byte{0x1C, 0x53, 0xBB, 0x6B})
	if start < 0 {
		t.Fatal("fixture has no Cues")
	}
	n := 0
	for i := start + 4; i+2 < len(out); i++ {
		switch {
		case out[i] == 0xF1 && out[i+1] > 0x80 && out[i+1] <= 0x88: // CueClusterPosition
			w := int(out[i+1] & 0x0F)
			out[i+2] = 0x7F
			for k := 1; k < w; k++ {
				out[i+2+k] = 0xFF
			}
			n++
			i += 1 + w
		case retrack && out[i] == 0xF7 && out[i+1] == 0x81 && out[i+2] == 0x01: // CueTrack 1
			out[i+2], retrack = 0x02, false
		}
	}
	if n == 0 {
		t.Fatal("no CueClusterPosition rewritten")
	}
	return out
}

// TestValidateCorruptCuesOnACompleteFile: cue points naming positions past the end of a file whose Segment ends
// within it are a corrupt index a reindex rewrites - not a cut file to re-download - and a video cue among them still
// counts as one: the index does not reference only non-video tracks.
func TestValidateCorruptCuesOnACompleteFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := range 4 {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: make([]byte, 64)},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: make([]byte, 16)},
		})
	}
	sealed := filepath.Join(dir, "sealed.mkv")
	if err := Reindex(ctx, buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 4000), sealed); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "corrupt.mkv")
	writeAll(t, bad, corruptCues(t, readAll(t, sealed), true))
	issues, err := Validate(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	stale := false
	for _, i := range issues {
		switch i.Code {
		case "cues-past-end":
			t.Errorf("a complete file's corrupt index reported as a cut: %s", i.Message)
		case "cues-non-video":
			if strings.HasPrefix(i.Message, "all ") {
				t.Errorf("video cues reported absent: %s", i.Message)
			}
		case "cues-stale":
			stale = stale || strings.Contains(i.Message, "reindex")
		}
	}
	if !stale {
		t.Errorf("no cues-stale issue sending to a reindex: %+v", issues)
	}
}
