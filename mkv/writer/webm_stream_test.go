package writer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

func TestNewWebMStreamWriter(t *testing.T) {
	opusHead := append([]byte("OpusHead"), 0x01, 0x02, 0x38, 0x01, 0x80, 0xbb, 0x00, 0x00, 0x00, 0x00, 0x00)
	info := mkv.SegmentInfo{TimecodeScale: 1_000_000}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "vp9"},
		{ID: 2, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead},
	}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01, 0x02}},
		{TrackNumber: 2, Timecode: 0, Keyframe: false, Data: []byte{0xAA}},
		{TrackNumber: 1, Timecode: 40, Keyframe: false, Data: []byte{0x03}},
	}

	var buf bytes.Buffer
	sw, err := NewWebMStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatalf("NewWebMStreamWriter: %v", err)
	}
	for _, b := range blocks {
		if err := sw.WriteBlock(b); err != nil {
			t.Fatalf("WriteBlock: %v", err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data := buf.Bytes()

	if dt, ver := ebmlHeaderInfo(t, data); dt != "webm" || ver != 2 {
		t.Errorf("header = (%q, v%d), want (webm, v2)", dt, ver)
	}

	// Read the complete stream back (frames included) via the streaming reader.
	c, br, err := reader.ReadStream(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Errorf("read back %d tracks, want 2", len(c.Tracks))
	}
	n := 0
	for {
		_, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("block read: %v", err)
		}
		n++
	}
	if n != len(blocks) {
		t.Errorf("read back %d blocks, want %d", n, len(blocks))
	}

	// A non-WebM codec is rejected up front.
	bad := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264"}}
	if _, err := NewWebMStreamWriter(io.Discard, info, bad); err == nil {
		t.Error("NewWebMStreamWriter accepted a non-WebM codec")
	}
}
