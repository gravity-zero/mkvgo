package writer

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestWriteClusterBlockGroupKeepsKeyframe: a block with a duration is written
// as a BlockGroup, and a Block has no keyframe flag - the group must name a
// ReferenceBlock for a frame that is not a keyframe. Without it every frame
// with a duration came back as a keyframe: a rewrite turned a file's P-frames
// into seek points.
func TestWriteClusterBlockGroupKeepsKeyframe(t *testing.T) {
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Duration: 40, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 2, Timecode: 0, Duration: 1500, Keyframe: true, Data: []byte("cue")},
		{TrackNumber: 1, Timecode: 40, Duration: 40, Keyframe: false, Data: []byte{0x02}},
		{TrackNumber: 1, Timecode: 80, Duration: 40, Keyframe: false, Data: []byte{0x03}},
		{TrackNumber: 1, Timecode: 120, Keyframe: false, Data: []byte{0x04}}, // no duration: a SimpleBlock
	}
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng"},
			{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}
	if err := WriteCluster(m.W, 0, 1000000, blocks); err != nil {
		t.Fatal(err)
	}

	br, err := reader.NewBlockReader(bytes.NewReader(buf.buf), 1000000)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range blocks {
		got, err := br.Next()
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if got.Keyframe != want.Keyframe || got.Duration != want.Duration || got.Timecode != want.Timecode {
			t.Errorf("block %d: keyframe=%v duration=%d timecode=%d, want keyframe=%v duration=%d timecode=%d",
				i, got.Keyframe, got.Duration, got.Timecode, want.Keyframe, want.Duration, want.Timecode)
		}
	}
	if _, err := br.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last block: %v, want EOF", err)
	}
}

// TestWriteSintElement pins the two's-complement widths of a ReferenceBlock.
func TestWriteSintElement(t *testing.T) {
	for _, tc := range []struct {
		val  int64
		want []byte
	}{
		{-1, []byte{0xFB, 0x81, 0xFF}},
		{-40, []byte{0xFB, 0x81, 0xD8}},
		{-128, []byte{0xFB, 0x81, 0x80}},
		{-129, []byte{0xFB, 0x82, 0xFF, 0x7F}},
		{127, []byte{0xFB, 0x81, 0x7F}},
		{128, []byte{0xFB, 0x82, 0x00, 0x80}},
		{-32768, []byte{0xFB, 0x82, 0x80, 0x00}},
	} {
		var b bytes.Buffer
		if err := writeSintElement(&b, mkv.IDReferenceBlock, tc.val); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b.Bytes(), tc.want) {
			t.Errorf("%d: wrote % X, want % X", tc.val, b.Bytes(), tc.want)
		}
	}
}

// TestWriteClusterKeepsLaces: frames read from one laced block of a track that
// states no frame duration all carry the block's timecode. Written back as
// separate blocks they all claimed the same instant - 3762 audio frames on 487
// distinct timestamps in a real rewrite. They go back into one laced block:
// fixed-size lacing when the frames are the same size, Xiph lacing otherwise
// (a frame of 255 bytes or more included), and the frames come back unchanged.
func TestWriteClusterKeepsLaces(t *testing.T) {
	big := bytes.Repeat([]byte{0x5A}, 600)
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
		// A lace of three frames of different sizes.
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Laced: true, Data: []byte{0xA1, 0xA2}},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Laced: true, Data: big},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Laced: true, Data: []byte{0xA3}},
		// The next lace of the same track: two frames of the same size.
		{TrackNumber: 2, Timecode: 64, Keyframe: true, Laced: true, Data: []byte{0xB1, 0xB2}},
		{TrackNumber: 2, Timecode: 64, Keyframe: true, Laced: true, Data: []byte{0xB3, 0xB4}},
		{TrackNumber: 1, Timecode: 40, Keyframe: false, Data: []byte{0x02}},
		// Laced at the source, but the reader knew the stride: frames with
		// their own timecodes stay separate blocks.
		{TrackNumber: 2, Timecode: 128, Keyframe: true, Laced: true, Data: []byte{0xC1}},
		{TrackNumber: 2, Timecode: 149, Keyframe: true, Laced: true, Data: []byte{0xC2}},
	}
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng"},
			{ID: 2, Type: mkv.AudioTrack, Codec: "vorbis", Language: "eng"},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}
	if err := WriteCluster(m.W, 0, 1000000, blocks); err != nil {
		t.Fatal(err)
	}

	br, err := reader.NewBlockReader(bytes.NewReader(buf.buf), 1000000)
	if err != nil {
		t.Fatal(err)
	}
	// Laced on the way back exactly where the frames shared a timecode.
	wantLaced := []bool{false, true, true, true, true, true, false, false, false}
	for i, want := range blocks {
		got, err := br.Next()
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if got.TrackNumber != want.TrackNumber || got.Timecode != want.Timecode || !bytes.Equal(got.Data, want.Data) || got.Keyframe != want.Keyframe {
			t.Errorf("block %d: track %d at %d ms, %d bytes, keyframe=%v; want track %d at %d ms, %d bytes, keyframe=%v",
				i, got.TrackNumber, got.Timecode, len(got.Data), got.Keyframe, want.TrackNumber, want.Timecode, len(want.Data), want.Keyframe)
		}
		if got.Laced != wantLaced[i] {
			t.Errorf("block %d: laced = %v, want %v", i, got.Laced, wantLaced[i])
		}
	}
	if _, err := br.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last block: %v, want EOF", err)
	}
}
