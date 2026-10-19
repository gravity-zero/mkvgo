package matrix

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestCasesAndCuts pins the matrix's shape and where its cuts really fall.
func TestCasesAndCuts(t *testing.T) {
	if n := len(Cases()); n != 240 {
		t.Errorf("%d cases, want 240", n)
	}
	if n := len(SecondVideoCases()); n != 20 {
		t.Errorf("%d second-video cases, want 20", n)
	}
	for _, tc := range []struct {
		c    Case
		want []int64
	}{
		{Case{Cuts: 1}, []int64{3000}},
		{Case{Cuts: 2}, []int64{2000, 4000}},
		{Case{Cuts: 1, NTSC: true}, []int64{3337}}, // 3336.67 ms: NTSC keyframes fall between milliseconds
		{Case{Cuts: 2, NTSC: true}, []int64{2503, 4171}},
		{Case{Cuts: 1, Late: true}, []int64{4000}},
		{Case{Cuts: 2, Late: true}, []int64{3000, 5000}},
		{Case{Cuts: 2, NTSC: true, Late: true}, []int64{2503, 5005}},
	} {
		if got := tc.c.ExpectedCuts(); len(got) != len(tc.want) || got[0] != tc.want[0] || got[len(got)-1] != tc.want[len(tc.want)-1] {
			t.Errorf("%s: cuts %v, want %v", tc.c.Name(), got, tc.want)
		}
	}
}

// TestBuildMKVStoresWhatBlocksSays: every case's file reads back, track by track, as Blocks states it, an open GOP's leading pictures behind their keyframe.
func TestBuildMKVStoresWhatBlocksSays(t *testing.T) {
	for _, c := range append(Cases(), SecondVideoCases()...) {
		path := filepath.Join(t.TempDir(), "src.mkv")
		if err := c.BuildMKV(path); err != nil {
			t.Fatalf("%s: %v", c.Name(), err)
		}
		got := readBlocks(t, path)
		for track, want := range c.Blocks() {
			if len(got[track]) != len(want) {
				t.Fatalf("%s track %d: %d blocks, want %d", c.Name(), track, len(got[track]), len(want))
			}
			for i, w := range want {
				g := got[track][i]
				if g.Timecode != w.Timecode || g.Keyframe != w.Keyframe || g.Duration != w.Duration || !bytes.Equal(g.Data, w.Data) {
					t.Fatalf("%s track %d block %d: %+v, want %+v", c.Name(), track, i, g, w)
				}
			}
		}
		if v := got[VideoTrack]; c.OpenGOP && (!v[c.gop()-2].Keyframe || v[c.gop()-1].Timecode >= v[c.gop()-2].Timecode) {
			t.Errorf("%s: the second keyframe is not stored ahead of its leading pictures", c.Name())
		}
	}
}

func readBlocks(t *testing.T, path string) map[uint64][]mkv.Block {
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
	out := map[uint64][]mkv.Block{}
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[b.TrackNumber] = append(out[b.TrackNumber], b)
	}
}
