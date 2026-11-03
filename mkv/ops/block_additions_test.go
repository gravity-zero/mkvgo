package ops

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// hdr10PlusT35 is the head of an HDR10+ ITU-T T.35 payload: country 0xB5, provider 0x003C, provider oriented 0x0001.
var hdr10PlusT35 = []byte{0xB5, 0x00, 0x3C, 0x00, 0x01, 0x04, 0x01, 0x40}

// additionsFixture is a Matroska of a VP9 track whose blocks carry an alpha plane (BlockAddID 1) and HDR10+ metadata
// (BlockAddID 4, mapped as ITU-T T.35), and a WebVTT track whose cues carry their settings (BlockAddID 1).
func additionsFixture(t *testing.T, dir string) string {
	t.Helper()
	vp9 := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: u32(64), Height: u32(64),
		BlockAdditionMappings: []mkv.BlockAdditionMapping{{Value: 4, Name: "hdr10plus", Type: mkv.BlockAddIDTypeITUT35}}}
	vtt := mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "webvtt", CodecPrivate: []byte("WEBVTT\n")}
	var sets [][]mkv.Block
	for c := range 4 {
		var set []mkv.Block
		for i := c * 10; i < c*10+10; i++ {
			set = append(set, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 100, Keyframe: i%10 == 0, Data: []byte{0x82, byte(i)},
				Additions: []mkv.BlockAddition{{ID: 1, Data: []byte{0xA1, byte(i)}}, {ID: 4, Data: append(append([]byte{}, hdr10PlusT35...), byte(i))}}})
			if i%5 == 0 {
				set = append(set, mkv.Block{TrackNumber: 2, Timecode: int64(i) * 100, Keyframe: true, Duration: 400, Data: []byte("cue"),
					Additions: []mkv.BlockAddition{{ID: 1, Data: []byte("line:90%\nid\n")}}})
			}
		}
		sets = append(sets, set)
	}
	return buildMultiClusterMKV(t, dir, "additions.mkv", []mkv.Track{vp9, vtt}, sets, 4000)
}

// readAdditions reads every block of path: per track, the additions of each block in order.
func readAdditions(t *testing.T, path string) (map[uint64][][]mkv.BlockAddition, *mkv.Container) {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint64][][]mkv.BlockAddition{}
	for {
		blk, err := br.Next()
		if err == io.EOF {
			return out, c
		}
		if err != nil {
			t.Fatal(err)
		}
		out[blk.TrackNumber] = append(out[blk.TrackNumber], blk.Additions)
	}
}

func sameAdditions(a, b []mkv.BlockAddition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
	}
	return true
}

// TestBlockAdditionsRead: the reader returns each block's additions with their IDs, and the track's T.35 mapping
// says HDR10+.
func TestBlockAdditionsRead(t *testing.T) {
	src := additionsFixture(t, t.TempDir())
	got, c := readAdditions(t, src)
	if len(got[1]) != 40 || len(got[2]) != 8 {
		t.Fatalf("%d video and %d subtitle blocks, want 40 and 8", len(got[1]), len(got[2]))
	}
	want := []mkv.BlockAddition{{ID: 1, Data: []byte{0xA1, 7}}, {ID: 4, Data: append(append([]byte{}, hdr10PlusT35...), 7)}}
	if !sameAdditions(got[1][7], want) {
		t.Errorf("video block 7 additions %+v, want %+v", got[1][7], want)
	}
	if !sameAdditions(got[2][0], []mkv.BlockAddition{{ID: 1, Data: []byte("line:90%\nid\n")}}) {
		t.Errorf("WebVTT cue additions %+v", got[2][0])
	}
	v := c.Tracks[0]
	if !v.HDR10Plus || len(v.BlockAdditionMappings) != 1 || v.BlockAdditionMappings[0].Type != mkv.BlockAddIDTypeITUT35 || v.BlockAdditionMappings[0].Name != "hdr10plus" {
		t.Errorf("track: HDR10Plus %v, mappings %+v", v.HDR10Plus, v.BlockAdditionMappings)
	}
}

// TestBlockAdditionsSurviveRewrites: every rewrite keeps each block's additions and the track's mapping - reindex,
// a metadata edit, a track removal, a merge, a split and the join of its parts.
func TestBlockAdditionsSurviveRewrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := additionsFixture(t, dir)
	want, _ := readAdditions(t, src)
	check := func(name, path string, tracks ...uint64) {
		t.Helper()
		got, c := readAdditions(t, path)
		for _, id := range tracks {
			if len(got[id]) != len(want[id]) {
				t.Errorf("%s: track %d has %d blocks, want %d", name, id, len(got[id]), len(want[id]))
				continue
			}
			for i := range want[id] {
				if !sameAdditions(got[id][i], want[id][i]) {
					t.Errorf("%s: track %d block %d additions %+v, want %+v", name, id, i, got[id][i], want[id][i])
					break
				}
			}
		}
		if v := c.Tracks[0]; !v.HDR10Plus || len(v.BlockAdditionMappings) != 1 {
			t.Errorf("%s: mapping lost: HDR10Plus %v, %+v", name, v.HDR10Plus, v.BlockAdditionMappings)
		}
	}
	out := func(n string) string { return filepath.Join(dir, n) }
	if err := Reindex(ctx, src, out("reindex.mkv")); err != nil {
		t.Fatal(err)
	}
	check("reindex", out("reindex.mkv"), 1, 2)
	if err := EditMetadata(ctx, src, out("edit.mkv"), func(c *mkv.Container) { c.Info.Title = "x" }); err != nil {
		t.Fatal(err)
	}
	check("edit-metadata", out("edit.mkv"), 1, 2)
	if err := RemoveTrack(ctx, src, out("remove.mkv"), []uint64{2}); err != nil {
		t.Fatal(err)
	}
	check("remove-track", out("remove.mkv"), 1)
	if err := Merge(ctx, mkv.MergeOptions{OutputPath: out("merge.mkv"), Inputs: []mkv.MergeInput{{SourcePath: src}}}); err != nil {
		t.Fatal(err)
	}
	check("merge", out("merge.mkv"), 1, 2)
	parts, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Join(ctx, parts, out("join.mkv")); err != nil {
		t.Fatal(err)
	}
	check("split+join", out("join.mkv"), 1, 2)
}

// TestWebVTTCueSettingsFromAdditions: a Matroska WebVTT cue's settings, stored in its BlockAddition, reach the
// WebVTT a conversion writes, on the cue's timing line.
func TestWebVTTCueSettingsFromAdditions(t *testing.T) {
	src := additionsFixture(t, t.TempDir())
	var b bytes.Buffer
	if err := ExtractSubtitleWebVTT(context.Background(), src, 2, &b); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b.Bytes(), []byte("00:00:00.000 --> 00:00:00.400 line:90%\ncue")) {
		t.Errorf("WebVTT lacks the cue settings:\n%s", b.String())
	}
}

// TestHDR10PlusFromBlockAdditions: a VP9 track without a T.35 mapping whose blocks carry HDR10+ in their additions
// is said HDR10+ by the in-band read, not by the head read alone.
func TestHDR10PlusFromBlockAdditions(t *testing.T) {
	dir := t.TempDir()
	vp9 := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Width: u32(64), Height: u32(64)}
	var set []mkv.Block
	for i := range 10 {
		set = append(set, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 100, Keyframe: i == 0, Data: []byte{0x82, byte(i)},
			Additions: []mkv.BlockAddition{{ID: 4, Data: hdr10PlusT35}}})
	}
	src := buildMultiClusterMKV(t, dir, "hdr10plus.mkv", []mkv.Track{vp9}, [][]mkv.Block{set}, 1000)
	head, err := reader.Open(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if head.Tracks[0].HDR10Plus {
		t.Error("the head read says HDR10+ without reading a block")
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c, err := reader.ReadMeta(context.Background(), f, src, reader.WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Tracks[0].HDR10Plus {
		t.Error("the in-band read does not see HDR10+ in the block additions")
	}
}
