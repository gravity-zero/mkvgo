package ops

// cueholeprobe_test.go pins what the hole probe pronounces on the three things
// a hole can hold - and, as firmly, when it refuses to pronounce: a cue whose
// position is not a cluster must never turn an empty walk into missing picture.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildMKVProbeFixture writes one cluster per set and cues every video keyframe
// that cueIt accepts at its REAL cluster position - what the probe seeks to.
func buildMKVProbeFixture(t *testing.T, dir, name string, tracks []mkv.Track, sets [][]mkv.Block, durationMs int64, cueIt func(mkv.Block) bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
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
	if err := mw.WriteMetadata(c, tracks, durationMs); err != nil {
		t.Fatal(err)
	}
	var cues []mkv.CuePoint
	for _, blocks := range sets {
		pos := mw.RelPos()
		for _, b := range blocks {
			if b.TrackNumber == 1 && b.Keyframe && cueIt(b) {
				cues = append(cues, mkv.CuePoint{TimeMs: b.Timecode, Track: 1, ClusterPos: pos})
			}
		}
		if err := writer.WriteCluster(f, blocks[0].Timecode, 1000000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	mw.Cues = cues
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// probeSets lays out a 300 s file, one 5 s cluster each: video keyframe + 4
// frames + audio everywhere, except inside (100 s, 200 s) where holeCluster
// decides what the cluster holds.
func probeSets(holeCluster func(ts int64) []mkv.Block) [][]mkv.Block {
	var sets [][]mkv.Block
	for ts := int64(0); ts < 300_000; ts += 5000 {
		if ts > 100_000 && ts < 200_000 {
			sets = append(sets, holeCluster(ts))
			continue
		}
		sets = append(sets, fullCluster(ts, true))
	}
	return sets
}

// fullCluster is 5 s of picture and sound starting at ts, keyframed or not.
func fullCluster(ts int64, keyframe bool) []mkv.Block {
	blocks := []mkv.Block{{TrackNumber: 1, Timecode: ts, Keyframe: keyframe, Data: []byte{0xAA}}}
	for i := int64(1); i < 5; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: ts + i*1000, Data: []byte{0xBB}})
	}
	for i := int64(0); i < 5; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: ts + i*1000, Keyframe: true, Data: []byte{0x01}})
	}
	return blocks
}

func audioOnlyCluster(ts int64) []mkv.Block {
	var blocks []mkv.Block
	for i := int64(0); i < 5; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: ts + i*1000, Keyframe: true, Data: []byte{0x01}})
	}
	return blocks
}

func TestProbeCueHolesClassifiesTheHole(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	outsideHole := func(b mkv.Block) bool { return b.Timecode <= 100_000 || b.Timecode >= 200_000 }
	all := func(mkv.Block) bool { return true }

	cases := []struct {
		name        string
		sets        [][]mkv.Block
		cueIt       func(mkv.Block) bool
		content     string
		remedyStart string
		detailHas   string
	}{
		// Keyframes every 5 s inside the hole, none of them cued.
		{"uncued keyframes", probeSets(func(ts int64) []mkv.Block { return fullCluster(ts, true) }), outsideHole,
			"uncued-keyframes", "mkvgo reindex", "holds uncued keyframes"},
		// Picture inside the hole, but not one keyframe: every cue there is.
		{"no keyframes", probeSets(func(ts int64) []mkv.Block { return fullCluster(ts, false) }), all,
			"no-keyframes", "re-encode the source", "no keyframe"},
		// Sound only inside the hole: the picture is missing from the stream.
		// The four frames of the GOP the opening cue starts are inside the hole
		// too and must not read as picture: 96 s without a video block do.
		{"no video", probeSets(audioOnlyCluster), all,
			"picture-missing", "re-acquire the source", "has no video at all for 96s of it"},
	}
	for _, tc := range cases {
		path := buildMKVProbeFixture(t, dir, tc.name+".mkv", tracks, tc.sets, 300_000, tc.cueIt)
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if r.Healthy || len(r.Holes) != 1 || r.Holes[0].AtMs != 100_000 || r.Holes[0].GapMs != 100_000 {
			t.Fatalf("%s: Healthy=%v Holes=%+v, want one 100 s hole at 100 s (%s)", tc.name, r.Healthy, r.Holes, r.Reason)
		}
		if err := ProbeCueHoles(ctx, path, r); err != nil {
			t.Fatalf("%s: probe: %v", tc.name, err)
		}
		if got := r.Holes[0].Content; got != tc.content {
			t.Errorf("%s: Content = %q (%+v), want %q", tc.name, got, r.Holes[0], tc.content)
		}
		if tc.content == "picture-missing" && r.Holes[0].VideoAbsentMs != 96_000 {
			t.Errorf("%s: VideoAbsentMs = %d, want 96000 (last frame at 104 s, far side at 200 s)", tc.name, r.Holes[0].VideoAbsentMs)
		}
		d, err := Diagnose(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		f := hasFinding(d, "index-sparse")
		if f == nil {
			t.Fatalf("%s: no index-sparse finding: %+v", tc.name, d.Findings)
		}
		if !strings.HasPrefix(f.Remedy, tc.remedyStart) || !strings.Contains(f.Detail, tc.detailHas) {
			t.Errorf("%s: finding = %q / %q, want remedy %q... and detail with %q", tc.name, f.Detail, f.Remedy, tc.remedyStart, tc.detailHas)
		}
	}
}

// TestProbeCueHolesTailAndMixed: the tail is probed to EOF (its natural end),
// and a file with one fixable hole and one hopeless one keeps the reindex as
// remedy - the fixable part gets fixed - while the detail names both.
func TestProbeCueHolesTailAndMixed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	// Picture to 100 s, sound to 250 s, declared 300 s: the 205 s tail counts
	// (no statistics tag, and far past the 5% an outlasting track accounts
	// for) and holds no video. Plus uncued keyframes between 50 and 85 s.
	var sets [][]mkv.Block
	for ts := int64(0); ts < 100_000; ts += 5000 {
		sets = append(sets, fullCluster(ts, true))
	}
	for ts := int64(100_000); ts < 250_000; ts += 5000 {
		sets = append(sets, audioOnlyCluster(ts))
	}
	path := buildMKVProbeFixture(t, dir, "mixed.mkv", tracks, sets, 300_000,
		func(b mkv.Block) bool { return b.Timecode <= 50_000 || b.Timecode >= 85_000 })

	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Holes) != 2 || r.Holes[0].AtMs != 50_000 || r.Holes[0].GapMs != 35_000 || r.Holes[1].AtMs != 95_000 || r.Holes[1].GapMs != 205_000 {
		t.Fatalf("Holes = %+v, want the 35 s hole at 50 s and the 205 s tail after 95 s", r.Holes)
	}
	if err := ProbeCueHoles(ctx, path, r); err != nil {
		t.Fatal(err)
	}
	if r.Holes[0].Content != "uncued-keyframes" || r.Holes[1].Content != "picture-missing" {
		t.Errorf("Contents = %q / %q, want uncued-keyframes / picture-missing (%+v)", r.Holes[0].Content, r.Holes[1].Content, r.Holes)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	f := hasFinding(d, "index-sparse")
	if f == nil || !strings.HasPrefix(f.Remedy, "mkvgo reindex (closes the hole(s) holding uncued keyframes") ||
		!strings.Contains(f.Detail, "205s tail after 00:01:35 has no video at all for 201s of it") {
		t.Errorf("finding = %+v, want the reindex kept for the fixable hole and the tail named as picture-less", f)
	}
}

// TestProbeCueHolesRefusesStalePositions: cues whose ClusterPos does not land
// on a cluster (the fixture writes fake positions) leave every hole
// unconcluded, and Diagnose keeps its head-only verdict.
func TestProbeCueHolesRefusesStalePositions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	path := buildMKVWithCuesDur(t, dir, "stale.mkv", tracks, cueHealthFixtureSets(4), cueSpread(1, 10, 60_000), 600_000)

	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Holes) == 0 {
		t.Fatal("want holes on a 1-cue-a-minute index")
	}
	if err := ProbeCueHoles(ctx, path, r); err != nil {
		t.Fatalf("a stale position must not fail the probe: %v", err)
	}
	for _, h := range r.Holes {
		if h.Content != "" {
			t.Errorf("hole %+v: pronounced %q from a position that is not a cluster", h, h.Content)
		}
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, "index-sparse"); f == nil || f.Remedy != "mkvgo reindex" || !strings.Contains(f.Detail, "run mkvgo reindex") {
		t.Errorf("finding = %+v, want the head-only verdict kept", f)
	}
}
