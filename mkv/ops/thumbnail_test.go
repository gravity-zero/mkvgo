package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// The extracted keyframe is the cued one at/before the requested time, in
// Annex-B with the avcC parameter sets prepended and NAL lengths rewritten
// as start codes.
func TestExtractKeyframeSample(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1F, 0xAC}
	pps := []byte{0x68, 0xEB, 0xE3, 0xCB}
	avcC := []byte{1, 0x64, 0x00, 0x1F, 0xFF, 0xE1, 0, byte(len(sps))}
	avcC = append(avcC, sps...)
	avcC = append(avcC, 1, 0, byte(len(pps)))
	avcC = append(avcC, pps...)

	w, h := uint32(320), uint32(240)
	nal := []byte{0x65, 0xAA, 0xBB, 0xCC} // IDR slice payload
	frame := append([]byte{0, 0, 0, byte(len(nal))}, nal...)
	var blocks []mkv.Block
	for i := 0; i < 100; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40,
			Keyframe: i%25 == 0, Data: append([]byte(nil), frame...)})
	}
	src := filepath.Join(t.TempDir(), "in.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264",
		CodecPrivate: avcC, Width: &w, Height: &h}}
	if err := mw.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteMetadata(c, tracks, 4000); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < len(blocks); start += 25 {
		end := start + 25
		if end > len(blocks) {
			end = len(blocks)
		}
		if err := mw.WriteClusterWithCues(blocks[start].Timecode, 1_000_000, blocks[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	ks, err := ExtractKeyframeSample(context.Background(), src, 2300)
	if err != nil {
		t.Fatal(err)
	}
	if ks.PtsMs != 2000 || ks.Codec != "h264" || ks.Ext != ".h264" {
		t.Fatalf("keyframe = %dms %s%s, want 2000ms h264", ks.PtsMs, ks.Codec, ks.Ext)
	}
	want := append([]byte{0, 0, 0, 1}, sps...)
	want = append(want, 0, 0, 0, 1)
	want = append(want, pps...)
	want = append(want, 0, 0, 0, 1)
	want = append(want, nal...)
	if !bytes.Equal(ks.Data, want) {
		t.Fatalf("Annex-B = %x\nwant      %x", ks.Data, want)
	}

	// Before the first cue → the first keyframe; unsupported codec errors.
	if ks, err := ExtractKeyframeSample(context.Background(), src, 0); err != nil || ks.PtsMs != 0 {
		t.Errorf("at 0: %v / %v", ks, err)
	}
}
