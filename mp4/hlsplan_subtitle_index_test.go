package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// subIndexSource is 600 s of video, a keyframe every two, under the subtitle blocks cue(i) gives for i in 0..199, each
// at its own time, written in block order in 2 s clusters: a cue point on every keyframe, as the full pass cuts.
func subIndexSource(t *testing.T, cue func(i int) mkv.Block) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subindex.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, h := uint32(320), uint32(240)
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
	}
	if err := mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "t", WritingApp: "t"}}, tracks, 600000); err != nil {
		t.Fatal(err)
	}
	subs := map[int64][]mkv.Block{}
	for i := 0; i < 200; i++ {
		b := cue(i)
		b.TrackNumber, b.Keyframe = 2, true
		subs[b.Timecode/2000] = append(subs[b.Timecode/2000], b)
	}
	for at := int64(0); at < 600; at += 2 {
		var blocks []mkv.Block
		for s := at; s < at+2; s++ {
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: s * 1000, Keyframe: s%2 == 0, Data: []byte{0, 0, 0, 1, 0x65, byte(s)}})
		}
		blocks = append(blocks, subs[at/2]...)
		if err := mw.WriteClusterWithCues(at*1000, 1000000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The windows the plan serves from its cue index are the full pass's, byte for byte: a cue stating a span of minutes
// reaches every segment it covers from far behind them, and cues out of time order fall back to every cue.
func TestHLSPlanSubtitleIndexMatchesFullPass(t *testing.T) {
	for name, cue := range map[string]func(i int) mkv.Block{
		"long cue": func(i int) mkv.Block {
			b := mkv.Block{Timecode: int64(i)*3000 + 100, Data: []byte(fmt.Sprintf("cue %d", i))}
			if i == 3 {
				b.Duration = 150000 // across some 75 segments of 2 s
			}
			return b
		},
		"out of order": func(i int) mkv.Block {
			at := int64(i)*3000 + 100
			if i%10 == 5 {
				at = int64(i-1)*3000 + 50 // in the cluster of the cue before it, written after it, starting before it
			}
			return mkv.Block{Timecode: at, Data: []byte(fmt.Sprintf("cue %d", i))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			src, dir := subIndexSource(t, cue), t.TempDir()
			if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000}); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000})
			if err != nil {
				t.Fatal(err)
			}
			for k := 0; k < plan.NumSegments(); k++ {
				res := fmt.Sprintf("sub1_%05d.vtt", k+1)
				got, _, err := plan.Resource(context.Background(), res)
				if err != nil {
					t.Fatal(err)
				}
				want, err := os.ReadFile(filepath.Join(dir, res))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s differs from the full pass:\nplan:\n%s\nfull:\n%s", res, got, want)
				}
			}
		})
	}
}
