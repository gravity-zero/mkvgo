package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildMKVWithCues builds a file whose Cues are supplied verbatim, so a
// defective index (audio-keyed, stale tracks) can be constructed at will.
func buildMKVWithCues(t *testing.T, dir, name string, tracks []mkv.Track, sets [][]mkv.Block, cues []mkv.CuePoint) string {
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
	if err := mw.WriteMetadata(c, tracks, 4000); err != nil {
		t.Fatal(err)
	}
	for _, blocks := range sets {
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

func cueHealthFixtureSets(n int) [][]mkv.Block {
	sets := make([][]mkv.Block, 0, n)
	for i := 0; i < n; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{0x01}},
		})
	}
	return sets
}

func TestCueHealth(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}

	cases := []struct {
		name       string
		cues       []mkv.CuePoint
		healthy    bool
		reasonHint string
	}{
		{"video-keyed index", []mkv.CuePoint{
			{TimeMs: 0, Track: 1, ClusterPos: 100}, {TimeMs: 1000, Track: 1, ClusterPos: 200},
		}, true, ""},
		{"audio-keyed index", []mkv.CuePoint{
			{TimeMs: 0, Track: 2, ClusterPos: 100}, {TimeMs: 1000, Track: 2, ClusterPos: 200}, {TimeMs: 2000, Track: 1, ClusterPos: 300},
		}, false, "non-video"},
		{"stale track reference", []mkv.CuePoint{
			{TimeMs: 0, Track: 9, ClusterPos: 100},
		}, false, "do not exist"},
		{"no index", nil, false, "no seek index"},
	}
	for _, tc := range cases {
		path := buildMKVWithCues(t, dir, strings.ReplaceAll(tc.name, " ", "_")+".mkv", tracks, cueHealthFixtureSets(4), tc.cues)
		r, err := CueHealth(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if r.Healthy != tc.healthy {
			t.Errorf("%s: Healthy = %v, want %v (%+v)", tc.name, r.Healthy, tc.healthy, r)
		}
		if !tc.healthy && !strings.Contains(r.Reason, tc.reasonHint) {
			t.Errorf("%s: reason %q does not mention %q", tc.name, r.Reason, tc.reasonHint)
		}
		if r.TotalCues != len(tc.cues) {
			t.Errorf("%s: TotalCues = %d, want %d", tc.name, r.TotalCues, len(tc.cues))
		}
	}

	// The audio-keyed case's numbers: 2 of 3 cues are non-video.
	path := buildMKVWithCues(t, dir, "pct.mkv", tracks, cueHealthFixtureSets(4), []mkv.CuePoint{
		{TimeMs: 0, Track: 2, ClusterPos: 100}, {TimeMs: 1000, Track: 2, ClusterPos: 200}, {TimeMs: 2000, Track: 1, ClusterPos: 300},
	})
	r, err := CueHealth(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if r.NonVideoCues != 2 || r.VideoCues != 1 || r.PerTrack[2] != 2 || r.LastCueMs != 2000 {
		t.Errorf("classification off: %+v", r)
	}

	// Audio-only files legitimately cue audio.
	audioOnly := buildMKVWithCues(t, dir, "audioonly.mkv", []mkv.Track{audioTrack(1)},
		[][]mkv.Block{{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}}}},
		[]mkv.CuePoint{{TimeMs: 0, Track: 1, ClusterPos: 100}})
	r, err = CueHealth(ctx, audioOnly)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Healthy || r.HasVideoTrack {
		t.Errorf("audio-only file with audio cues must be healthy: %+v", r)
	}
}
