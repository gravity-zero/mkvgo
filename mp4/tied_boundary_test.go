package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// A frame that is no keyframe carries the timestamp of the keyframe after it, and two frames that present before it
// follow it in decode order, in the cluster before the keyframe's - how a muxer that opens a cluster on each keyframe
// stores them. All three belong to the segment they decode in, and the next one opens on its keyframe. Cut on time
// alone, the full pass opened the next segment on the tied frame, and a window served cold lost the two after it.
func TestSegmentOpensOnItsKeyframeWhenAFrameSharesItsTimestamp(t *testing.T) {
	w, h := uint32(320), uint32(240)
	payload := func(i int) []byte { return []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x7E, byte(i), 0x7E, byte(i), 0x7E} }
	path := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	m := writer.NewMKVWriter(f)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, 74*40); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 3; g++ { // one cluster a GOP of 25 frames, 40 ms each
		var blks []mkv.Block
		for i := g * 25; i < g*25+25; i++ {
			pts := int64(i) * 40
			switch i { // decode order: 47 stamped like keyframe 50, then 48 and 49 presenting before it
			case 47:
				pts = 2000
			case 48:
				pts = 1880
			case 49:
				pts = 1920
			}
			blks = append(blks, mkv.Block{TrackNumber: 1, Timecode: pts, Keyframe: i%25 == 0, Data: payload(i)})
		}
		if err := m.WriteClusterWithCues(int64(g)*1000, scale, blks); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, path, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		plan, err := PlanHLS(ctx, path, Options{SegmentMs: 2000}) // a fresh plan: the window is served cold
		if err != nil {
			t.Fatal(err)
		}
		if plan.NumSegments() != 2 {
			t.Fatalf("plan has %d segments, want 2", plan.NumSegments())
		}
		full, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
		if err != nil {
			t.Fatal(err)
		}
		od, err := plan.Segment(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range []int{47, 48, 49, 50} {
			want := (i < 50) == (n == 0)
			if got := bytes.Contains(full, payload(i)[4:]); got != want {
				t.Errorf("full pass: segment %d holds frame %d = %v, want %v", n, i, got, want)
			}
			if got := bytes.Contains(od, payload(i)[4:]); got != want {
				t.Errorf("on demand: segment %d holds frame %d = %v, want %v", n, i, got, want)
			}
		}
		if !bytes.Equal(od, full) {
			t.Errorf("segment %d: on demand differs from the full pass (%d vs %d bytes)", n, len(od), len(full))
		}
	}
}
