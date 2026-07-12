package reader

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// buildCuedMKV writes a real MKV (SeekHead + Cues + clusters) with one cluster  -
// and thus one cue point - at each of the given timecodes (ms, default scale).
func buildCuedMKV(t *testing.T, clusterTimecodes []int64) []byte {
	t.Helper()
	f, err := createTempFile(t)
	if err != nil {
		t.Fatal(err)
	}
	defer removeTempFile(f)

	track := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264"}
	mw := writer.NewMKVWriter(f)
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	cont := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}
	if err := mw.WriteMetadata(cont, []mkv.Track{track}, 10000); err != nil {
		t.Fatal(err)
	}
	for _, tc := range clusterTimecodes {
		blocks := []mkv.Block{{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte{1, 2, 3}}}
		if err := mw.WriteClusterWithCues(tc, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestKeyframesMKVFromMeta checks ReadMeta fills Container.Keyframes from the Cues
// seek index in the same pass, leaving Cues themselves nil (the meta contract).
func TestKeyframesMKVFromMeta(t *testing.T) {
	data := buildCuedMKV(t, []int64{0, 2000, 4000, 6000})
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	want := []int64{0, 2000, 4000, 6000}
	if len(c.Keyframes) != len(want) {
		t.Fatalf("Keyframes = %v, want %v", c.Keyframes, want)
	}
	for i := range want {
		if c.Keyframes[i] != want[i] {
			t.Fatalf("Keyframes = %v, want %v", c.Keyframes, want)
		}
	}
	if c.Cues != nil {
		t.Errorf("ReadMeta must leave Cues nil, got %v", c.Cues)
	}
}

// TestKeyframesMKVFullRead checks a full Read also exposes Keyframes (derived from
// the Cues it parses).
func TestKeyframesMKVFullRead(t *testing.T) {
	data := buildCuedMKV(t, []int64{0, 2000, 4000})
	c, err := Read(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Keyframes) != 3 || c.Keyframes[0] != 0 || c.Keyframes[2] != 4000 {
		t.Errorf("Keyframes = %v, want [0 2000 4000]", c.Keyframes)
	}
}

// TestKeyframesMKVNoCues checks a file with no Cues index leaves Keyframes nil (the
// caller can then fall back to a packet scan).
func TestKeyframesMKVNoCues(t *testing.T) {
	// buildPlainMKV (meta_test.go) builds [Info][Tracks][Cluster] with no Cues.
	c, err := ReadMeta(context.Background(), bytes.NewReader(buildPlainMKV()), "x.mkv")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if c.Keyframes != nil {
		t.Errorf("Keyframes = %v, want nil for a file without Cues", c.Keyframes)
	}
}
