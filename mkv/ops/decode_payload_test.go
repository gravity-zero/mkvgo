package ops

import (
	"bytes"
	"compress/zlib"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func zlibOf(raw []byte) []byte {
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	return zb.Bytes()
}

// A zlib-compressed audio track demuxes to its inflated frames, and a
// zlib-compressed video track hands its keyframe out inflated.
func TestDemuxAndKeyframeInflateCompressedTracks(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	frame := append([]byte{0, 0, 1, 0x2D, 0x65}, bytes.Repeat([]byte{7}, 300)...) // one 301-byte IDR NAL, length-prefixed
	audio := bytes.Repeat([]byte{0x11, 0x22, 0x33}, 100)
	var blocks []mkv.Block
	for i := 0; i < 50; i++ {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: int64(i) * 40, Keyframe: i%25 == 0, Data: zlibOf(frame)})
		blocks = append(blocks, mkv.Block{TrackNumber: 2, Timecode: int64(i) * 40, Keyframe: true, Data: zlibOf(audio)})
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Width: &w, Height: &h, Compression: mkv.CompressionZlib},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", SampleRate: &sr, Compression: mkv.CompressionZlib},
	}
	dir := t.TempDir()
	src := buildMKVWithCues(t, dir, "z.mkv", tracks, [][]mkv.Block{blocks[:50], blocks[50:]}, []mkv.CuePoint{{TimeMs: 0, Track: 1}})
	out := filepath.Join(dir, "out")
	if err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: out, TrackIDs: []uint64{2}}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "*"))
	if len(files) != 1 {
		t.Fatalf("demux wrote %v, want one file", files)
	}
	got, _ := os.ReadFile(files[0])
	if !bytes.Equal(got, bytes.Repeat(audio, 50)) {
		t.Errorf("demuxed audio: %d bytes, want 50 inflated frames of %d", len(got), len(audio))
	}
	ks, err := ExtractKeyframeSample(context.Background(), src, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(ks.Data, frame[4:]) { // the NAL itself: the sample is re-framed for a decoder
		t.Errorf("keyframe sample (%d bytes) does not hold the inflated frame", len(ks.Data))
	}
}
