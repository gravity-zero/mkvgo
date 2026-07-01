package writer

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// A source authored with a non-1ms TimecodeScale used to come back wrong:
// WriteCluster converted the cluster Timestamp to timecode-scale units but
// wrote the per-block offset as raw milliseconds, so any scale other than
// 1_000_000 silently skewed block presentation times toward the cluster start.
func TestWriteClusterNonDefaultTimecodeScale(t *testing.T) {
	const scale = int64(500_000) // 0.5 ms units

	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "t", WritingApp: "t"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true},
			{ID: 2, Type: mkv.SubtitleTrack, Codec: "S_TEXT/UTF8", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}

	// Cluster at 5000 ms; blocks at +0 ms and +33 ms, plus a subtitle cue with
	// an explicit duration (BlockGroup path).
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 5000, Keyframe: true, Data: []byte{0xAA}},
		{TrackNumber: 1, Timecode: 5033, Keyframe: false, Data: []byte{0xBB}},
		{TrackNumber: 2, Timecode: 5100, Keyframe: true, Duration: 2000, Data: []byte("sub")},
	}
	if err := WriteCluster(m.W, 5000, scale, blocks); err != nil {
		t.Fatal(err)
	}

	br, err := reader.NewBlockReader(bytes.NewReader(buf.buf), scale)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ tc, dur int64 }{{5000, 0}, {5033, 0}, {5100, 2000}}
	for i, w := range want {
		b, err := br.Next()
		if err != nil {
			t.Fatalf("block[%d]: %v", i, err)
		}
		if b.Timecode != w.tc {
			t.Errorf("block[%d] Timecode = %d, want %d", i, b.Timecode, w.tc)
		}
		if w.dur > 0 && b.Duration != w.dur {
			t.Errorf("block[%d] Duration = %d, want %d", i, b.Duration, w.dur)
		}
	}
}

// Same unit bug in the live streaming path: relativeTimecode returned the raw
// millisecond delta regardless of the declared TimecodeScale.
func TestStreamWriterNonDefaultTimecodeScale(t *testing.T) {
	const scale = int64(500_000)

	var buf bytes.Buffer
	info := mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "t", WritingApp: "t"}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", IsDefault: true}}
	sw, err := NewStreamWriter(&buf, info, tracks)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 1, Timecode: 40, Data: []byte{0x02}},
		{TrackNumber: 1, Timecode: 80, Data: []byte{0x03}},
	} {
		if err := sw.WriteBlockInCurrentCluster(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}

	br, err := reader.NewStreamBlockReader(bytes.NewReader(buf.Bytes()), scale)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{0, 40, 80} {
		b, err := br.Next()
		if err != nil {
			t.Fatalf("block[%d]: %v", i, err)
		}
		if b.Timecode != want {
			t.Errorf("block[%d] Timecode = %d, want %d", i, b.Timecode, want)
		}
	}
}

// A block offset that does not fit a SimpleBlock's int16 (once expressed in
// timecode-scale units) must be an explicit error, not a silent int16 wrap.
func TestWriteClusterRelTCOverflow(t *testing.T) {
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 40_000, Keyframe: true, Data: []byte{0x01}},
	}
	err := WriteCluster(&bytes.Buffer{}, 0, 1_000_000, blocks)
	if err == nil {
		t.Fatal("expected error for 40s block offset, got nil")
	}
	if !strings.Contains(err.Error(), "int16") {
		t.Errorf("error should mention the int16 range, got: %v", err)
	}

	// Finer scale shrinks the encodable window: +20 s at 0.5 ms units = 40000.
	blocks[0].Timecode = 20_000
	if err := WriteCluster(&bytes.Buffer{}, 0, 500_000, blocks); err == nil {
		t.Fatal("expected error for +20s offset at scale 500_000, got nil")
	}
}

// TimecodeScale == 0 means "unset": WriteSegmentInfo falls back to the
// Matroska default (1_000_000) instead of dropping both the scale and the
// Duration derived from durationMs.
func TestWriteSegmentInfoZeroScaleDuration(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info:   mkv.SegmentInfo{MuxingApp: "t", WritingApp: "t"}, // TimecodeScale unset
		Tracks: []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", IsDefault: true}},
	}
	if err := m.WriteMetadata(c, c.Tracks, 5000); err != nil {
		t.Fatal(err)
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}

	got, err := reader.Read(context.Background(), bytes.NewReader(buf.buf), "test.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.TimecodeScale != 1_000_000 {
		t.Errorf("TimecodeScale = %d, want 1_000_000", got.Info.TimecodeScale)
	}
	if got.DurationMs != 5000 {
		t.Errorf("DurationMs = %d, want 5000", got.DurationMs)
	}
}
