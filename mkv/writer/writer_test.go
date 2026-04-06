package writer

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

func TestWriteAndReadBack(t *testing.T) {
	w := uint32(1920)
	h := uint32(1080)
	sr := 48000.0
	ch := uint8(2)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: 1000000,
			Title:         "Round Trip",
			MuxingApp:     "test",
			WritingApp:    "test",
		},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "opus", Language: "eng", SampleRate: &sr, Channels: &ch},
		},
		DurationMs: 5000,
	}

	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatalf("Write: %v", err)
	}

	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "test.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Info.Title != "Round Trip" {
		t.Errorf("title = %q", got.Info.Title)
	}
	if len(got.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(got.Tracks))
	}
}

func TestWriteClusterAndReadBlocks(t *testing.T) {
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0xDE, 0xAD}},
		{TrackNumber: 1, Timecode: 33, Keyframe: false, Data: []byte{0xBE, 0xEF}},
	}

	// Build a valid MKV with EBML header + Segment(unknown size) + Cluster
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}
	if err := WriteCluster(m.W, 0, 1000000, blocks); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(buf.buf)
	br, err := reader.NewBlockReader(r, 1000000)
	if err != nil {
		t.Fatal(err)
	}

	b1, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !b1.Keyframe {
		t.Error("first block should be keyframe")
	}
	if !bytes.Equal(b1.Data, []byte{0xDE, 0xAD}) {
		t.Errorf("block 0 data = %x", b1.Data)
	}

	b2, err := br.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b2.Keyframe {
		t.Error("second block should not be keyframe")
	}
}

func TestWriteCues(t *testing.T) {
	cues := []mkv.CuePoint{
		{TimeMs: 0, Track: 1, ClusterPos: 0},
		{TimeMs: 5000, Track: 1, ClusterPos: 10000},
	}
	var buf bytes.Buffer
	if err := WriteCues(&buf, cues, 1000000); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("WriteCues produced empty output")
	}
}

func TestWriteSeekHead(t *testing.T) {
	entries := []SeekEntry{
		{ID: mkv.IDInfo, Pos: 100},
		{ID: mkv.IDTracks, Pos: 200},
		{ID: mkv.IDCues, Pos: 5000},
	}
	var buf bytes.Buffer
	if err := WriteSeekHead(&buf, entries); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("WriteSeekHead produced empty output")
	}
}

func TestWriteVoid(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"too small", 1},
		{"minimum", 2},
		{"small", 10},
		{"medium", 100},
		{"large", 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteVoid(&buf, tt.size); err != nil {
				t.Fatal(err)
			}
			if tt.size < 2 {
				if buf.Len() != 0 {
					t.Errorf("size %d should produce empty output, got %d bytes", tt.size, buf.Len())
				}
				return
			}
			if buf.Len() != tt.size {
				t.Errorf("WriteVoid(%d) produced %d bytes", tt.size, buf.Len())
			}
		})
	}
}

func TestEncodeElementID(t *testing.T) {
	tests := []struct {
		name string
		id   uint32
		want int // expected byte count
	}{
		{"1-byte", 0xEC, 1},       // IDVoid
		{"2-byte", 0x4489, 2},     // IDDuration
		{"3-byte", 0x2AD7B1, 3},   // IDTimecodeScale
		{"4-byte", 0x18538067, 4}, // IDSegment
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EncodeElementID(tt.id)
			if len(got) != tt.want {
				t.Errorf("EncodeElementID(0x%X) = %d bytes, want %d", tt.id, len(got), tt.want)
			}
			// Verify round-trip: reconstruct the ID
			var val uint32
			for _, b := range got {
				val = (val << 8) | uint32(b)
			}
			if val != tt.id {
				t.Errorf("round-trip: got 0x%X, want 0x%X", val, tt.id)
			}
		})
	}
}

func TestCodecIDFromShort(t *testing.T) {
	tests := []struct {
		short string
		want  string
	}{
		{"h264", "V_MPEG4/ISO/AVC"},
		{"opus", "A_OPUS"},
		{"srt", "S_TEXT/UTF8"},
		{"unknown_codec", "unknown_codec"},
	}
	for _, tt := range tests {
		got := CodecIDFromShort(tt.short)
		if got != tt.want {
			t.Errorf("CodecIDFromShort(%q) = %q, want %q", tt.short, got, tt.want)
		}
	}
}

func TestMKVWriterFullWorkflow(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)

	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Ch1", StartMs: 0, EndMs: 5000},
		},
		Tags: []mkv.Tag{
			{SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Test"}}},
		},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "f.ttf", MIMEType: "font/ttf", Data: []byte{0x00}},
		},
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true},
	}

	if err := m.WriteMetadata(c, tracks, 10000); err != nil {
		t.Fatal(err)
	}

	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
	}
	if err := m.WriteClusterWithCues(0, 1000000, blocks); err != nil {
		t.Fatal(err)
	}

	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}

	if m.InfoPos == 0 {
		t.Error("InfoPos not set")
	}
	if m.TracksPos == 0 {
		t.Error("TracksPos not set")
	}
	if m.ChaptersPos == 0 {
		t.Error("ChaptersPos not set")
	}
	if m.TagsPos == 0 {
		t.Error("TagsPos not set")
	}
	if m.AttachPos == 0 {
		t.Error("AttachPos not set")
	}
	if len(m.Cues) != 1 {
		t.Errorf("cues = %d, want 1", len(m.Cues))
	}
}

func TestMKVWriterRelPos(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	rp := m.RelPos()
	if rp < 0 {
		t.Errorf("RelPos = %d, want >= 0", rp)
	}
}

func TestWriteAttachments(t *testing.T) {
	atts := []mkv.Attachment{
		{ID: 1, Name: "a.ttf", MIMEType: "font/ttf", Data: []byte{0x01, 0x02}},
		{ID: 2, Name: "b.png", MIMEType: "image/png", Data: []byte{0x89, 0x50}},
	}
	var buf bytes.Buffer
	if err := WriteAttachments(&buf, atts); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}
}

func TestWriteChaptersWithSegmentUID(t *testing.T) {
	chapters := []mkv.Chapter{
		{ID: 1, Title: "Ch1", StartMs: 0, EndMs: 5000, SegmentUID: []byte{0xAA, 0xBB}},
	}
	var buf bytes.Buffer
	if err := WriteChapters(&buf, chapters); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}
}

func TestWriteSegmentInfoWithUIDs(t *testing.T) {
	var buf bytes.Buffer
	info := &mkv.SegmentInfo{
		TimecodeScale: 1000000,
		MuxingApp:     "test",
		WritingApp:    "test",
		SegmentUID:    []byte{0x01, 0x02, 0x03},
		PrevUID:       []byte{0x04, 0x05},
		NextUID:       []byte{0x06, 0x07},
	}
	if err := WriteSegmentInfo(&buf, info, 5000); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}
}

func TestWriteTagsWithLanguage(t *testing.T) {
	tags := []mkv.Tag{
		{
			TargetType: "MOVIE",
			TargetID:   1,
			SimpleTags: []mkv.SimpleTag{
				{Name: "TITLE", Value: "Movie", Language: "eng"},
			},
		},
	}
	var buf bytes.Buffer
	if err := WriteTags(&buf, tags); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}
}

func TestWriteSimpleBlock(t *testing.T) {
	var buf bytes.Buffer
	err := WriteSimpleBlock(&buf, 1, 0, true, []byte{0xAA, 0xBB})
	if err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty output")
	}
}

func TestHeaderStrippingRoundTrip(t *testing.T) {
	headerBytes := []byte{0x00, 0x00, 0x00, 0x01}
	bd := uint8(16)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tracks: []mkv.Track{
			{
				ID: 1, Type: mkv.AudioTrack, Codec: "flac", Language: "eng",
				IsDefault: true, HeaderStripping: headerBytes, BitDepth: &bd,
			},
		},
	}

	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "test.mkv")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got.Tracks[0].HeaderStripping, headerBytes) {
		t.Errorf("HeaderStripping = %x, want %x", got.Tracks[0].HeaderStripping, headerBytes)
	}
}

func TestWriteWithErrWriter(t *testing.T) {
	w := uint32(1920)
	h := uint32(1080)
	sr := 48000.0
	ch := uint8(2)
	bd := uint8(16)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: 1000000,
			Title:         "Test",
			Duration:      10000.0,
		},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true,
				Width: &w, Height: &h, Name: "Video", CodecPrivate: []byte{0x01},
				HeaderStripping: []byte{0x00, 0x00, 0x01}},
			{ID: 2, Type: mkv.AudioTrack, Codec: "opus", Language: "fre", IsDefault: false,
				IsForced: true, SampleRate: &sr, Channels: &ch, BitDepth: &bd},
		},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Ch1", StartMs: 0, EndMs: 5000, SegmentUID: []byte{0xAA}},
		},
		Tags: []mkv.Tag{{
			TargetType: "MOVIE", TargetID: 1,
			SimpleTags: []mkv.SimpleTag{
				{Name: "TITLE", Value: "Movie", Language: "eng", Binary: []byte{0xFF},
					SubTags: []mkv.SimpleTag{{Name: "SUB", Value: "val"}}},
			},
		}},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "f.ttf", MIMEType: "font/ttf", Data: []byte{0x01}},
		},
	}

	// First, get the full size
	var full bytes.Buffer
	if err := Write(&full, c); err != nil {
		t.Fatal(err)
	}

	// Test error at every byte position
	for limit := 0; limit < full.Len(); limit++ {
		ew := &errWriter{limit: limit}
		Write(ew, c) // errors expected, just no panics
	}
}

func TestWriteAllBranches(t *testing.T) {
	w := uint32(1920)
	h := uint32(1080)
	sr := 48000.0
	ch := uint8(2)
	bd := uint8(16)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: 1000000,
			Title:         "Test",
			Duration:      10000.0,
		},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true,
				Width: &w, Height: &h, Name: "Video", CodecPrivate: []byte{0x01},
				HeaderStripping: []byte{0x00, 0x00, 0x01}},
			{ID: 2, Type: mkv.AudioTrack, Codec: "opus", Language: "fre", IsDefault: false,
				IsForced: true, SampleRate: &sr, Channels: &ch, BitDepth: &bd},
			{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng"},
		},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Ch1", StartMs: 0, EndMs: 5000, SegmentUID: []byte{0xAA}},
		},
		Tags: []mkv.Tag{{
			TargetType: "MOVIE",
			TargetID:   1,
			SimpleTags: []mkv.SimpleTag{
				{Name: "TITLE", Value: "Movie", Language: "eng", Binary: []byte{0xFF},
					SubTags: []mkv.SimpleTag{{Name: "SUB", Value: "val"}}},
			},
		}},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "f.ttf", MIMEType: "font/ttf", Data: []byte{0x01}},
		},
		DurationMs: 10000,
	}

	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}

	// Read it back to verify
	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "all.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 3 {
		t.Errorf("tracks = %d", len(got.Tracks))
	}
	if got.Tracks[0].Name != "Video" {
		t.Errorf("track 0 name = %q", got.Tracks[0].Name)
	}
}

func TestWriteSegmentInfoDurationFromMs(t *testing.T) {
	var buf bytes.Buffer
	info := &mkv.SegmentInfo{TimecodeScale: 1000000}
	if err := WriteSegmentInfo(&buf, info, 5000); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty")
	}
}

func TestWriteSegmentInfoDefaultApps(t *testing.T) {
	var buf bytes.Buffer
	info := &mkv.SegmentInfo{TimecodeScale: 1000000}
	if err := WriteSegmentInfo(&buf, info, 0); err != nil {
		t.Fatal(err)
	}
	// Should have "mkvgo" as default MuxingApp/WritingApp
	if buf.Len() == 0 {
		t.Fatal("empty")
	}
}

func TestMKVWriterFinalizeSeekHeadOverflow(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
	}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}

	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
}

func TestMKVWriterNoCues(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}
	// No clusters written, so no cues
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	if len(m.Cues) != 0 {
		t.Errorf("cues = %d, want 0", len(m.Cues))
	}
}

func TestWriteClusterWithCuesAudioOnly(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
	}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}

	// Blocks without keyframe — audio-only fallback adds cue for first block
	blocks := []mkv.Block{
		{TrackNumber: 2, Timecode: 0, Keyframe: false, Data: []byte{0x01}},
	}
	if err := m.WriteClusterWithCues(0, 1000000, blocks); err != nil {
		t.Fatal(err)
	}
	if len(m.Cues) != 1 {
		t.Errorf("cues = %d, want 1 (audio-only fallback)", len(m.Cues))
	}

	// Second cluster within 500ms — no additional cue
	blocks2 := []mkv.Block{
		{TrackNumber: 2, Timecode: 100, Keyframe: false, Data: []byte{0x02}},
	}
	if err := m.WriteClusterWithCues(100, 1000000, blocks2); err != nil {
		t.Fatal(err)
	}
	if len(m.Cues) != 1 {
		t.Errorf("cues = %d, want 1 (within interval)", len(m.Cues))
	}

	// Third cluster at 600ms — should add a cue
	blocks3 := []mkv.Block{
		{TrackNumber: 2, Timecode: 600, Keyframe: false, Data: []byte{0x03}},
	}
	if err := m.WriteClusterWithCues(600, 1000000, blocks3); err != nil {
		t.Fatal(err)
	}
	if len(m.Cues) != 2 {
		t.Errorf("cues = %d, want 2 (past interval)", len(m.Cues))
	}
}

func TestEwErrorPropagation(t *testing.T) {
	// Test that ew methods short-circuit when err is already set
	e := &ew{}
	e.err = io.ErrShortWrite

	e.uint(0xD7, 1)
	if e.err != io.ErrShortWrite {
		t.Error("uint should propagate error")
	}

	e.float64(0x4489, 1.0)
	if e.err != io.ErrShortWrite {
		t.Error("float64 should propagate error")
	}

	e.str(0x86, "test")
	if e.err != io.ErrShortWrite {
		t.Error("str should propagate error")
	}

	e.raw(0x63A2, []byte{0x01})
	if e.err != io.ErrShortWrite {
		t.Error("raw should propagate error")
	}

	e.master(0xAE, func(child *ew) {
		child.uint(0xD7, 1)
	})
	if e.err != io.ErrShortWrite {
		t.Error("master should propagate error")
	}

	err := e.flush(&bytes.Buffer{}, 0xAE)
	if err != io.ErrShortWrite {
		t.Errorf("flush should return error, got %v", err)
	}
}

func TestMasterChildError(t *testing.T) {
	e := &ew{}
	e.master(0xAE, func(child *ew) {
		child.err = io.ErrShortWrite
	})
	if e.err != io.ErrShortWrite {
		t.Error("master should propagate child error")
	}
}

func TestWriteElementsWithErrWriter(t *testing.T) {
	ew0 := &errWriter{limit: 0}

	if err := WriteUintElement(ew0, 0xD7, 1); err == nil {
		t.Error("expected error from WriteUintElement")
	}

	ew0 = &errWriter{limit: 0}
	if err := WriteFloatElement(ew0, 0x4489, 1.0); err == nil {
		t.Error("expected error from WriteFloatElement")
	}

	ew0 = &errWriter{limit: 0}
	if err := WriteStringElement(ew0, 0x86, "test"); err == nil {
		t.Error("expected error from WriteStringElement")
	}

	ew0 = &errWriter{limit: 0}
	if err := WriteBytesElement(ew0, 0x63A2, []byte{0x01}); err == nil {
		t.Error("expected error from WriteBytesElement")
	}

	// Test with limit that allows header but not data (2 bytes header, then fail on data)
	ew2 := &errWriter{limit: 2}
	if err := WriteUintElement(ew2, 0xD7, 1); err == nil {
		t.Error("expected error from WriteUintElement with partial write")
	}

	ew2 = &errWriter{limit: 2}
	if err := WriteFloatElement(ew2, 0xB5, 1.0); err == nil {
		t.Error("expected error from WriteFloatElement with partial write")
	}

	ew2 = &errWriter{limit: 2}
	if err := WriteStringElement(ew2, 0x86, "test"); err == nil {
		t.Error("expected error from WriteStringElement with partial write")
	}

	ew2 = &errWriter{limit: 2}
	if err := WriteBytesElement(ew2, 0x86, []byte{0x01, 0x02, 0x03}); err == nil {
		t.Error("expected error from WriteBytesElement with partial write")
	}
}

func TestWriteSimpleBlockWithErrWriter(t *testing.T) {
	for limit := 0; limit < 20; limit++ {
		ew := &errWriter{limit: limit}
		WriteSimpleBlock(ew, 1, 0, true, []byte{0xAA})
	}
}

func TestWriteVoidWithErrWriter(t *testing.T) {
	ew := &errWriter{limit: 2}
	err := WriteVoid(ew, 10)
	if err == nil {
		t.Error("expected error")
	}
}

func TestWriteClusterWithErrWriter(t *testing.T) {
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
	}
	for limit := 0; limit < 30; limit++ {
		ew := &errWriter{limit: limit}
		WriteCluster(ew, 0, 1000000, blocks)
	}
}

func TestWriteSegmentInfoWithDateUTC(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	info := &mkv.SegmentInfo{
		TimecodeScale: 1000000,
		DateUTC:       &now,
	}
	if err := WriteSegmentInfo(&buf, info, 0); err != nil {
		t.Fatal(err)
	}
}

func TestMKVWriterErrorPaths(t *testing.T) {
	// WriteStart with errSeekWriter that fails at various points
	for limit := 0; limit < 100; limit++ {
		buf := &errSeekBuffer{limit: limit}
		m := NewMKVWriter(buf)
		m.WriteStart() // errors expected
	}
}

func TestMKVWriterWriteMetadataErrors(t *testing.T) {
	// Get a working MKVWriter past WriteStart
	var goodBuf seekBuffer
	m := NewMKVWriter(&goodBuf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 0}, // triggers default
	}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}
	if m.TimecodeScale != 1000000 {
		t.Errorf("default timecode scale = %d, want 1000000", m.TimecodeScale)
	}
}

func TestWriteFull(t *testing.T) {
	// Test Write with all optional sections
	w := uint32(1920)
	h := uint32(1080)
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true, Width: &w, Height: &h},
		},
		Chapters:    []mkv.Chapter{{ID: 1, Title: "Ch", StartMs: 0}},
		Attachments: []mkv.Attachment{{ID: 1, Name: "f", MIMEType: "a/b", Data: []byte{0x01}}},
		Tags:        []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "N", Value: "V"}}}},
	}

	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}

	// Now test Write with errWriter at each position
	for limit := 0; limit < buf.Len(); limit++ {
		ew := &errWriter{limit: limit}
		Write(ew, c) // errors expected
	}
}

func TestMKVWriterFinalizeSeekHeadTooLarge(t *testing.T) {
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	// Set many positions to create a large seek head
	m.InfoPos = 100
	m.TracksPos = 200
	m.ChaptersPos = 300
	m.AttachPos = 400
	m.TagsPos = 500
	m.CuesPos = 600
	m.TimecodeScale = 1000000
	// This should fit, but let's also test the error paths in Finalize
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
}

func TestMKVWriterFinalizeWithErrSeekWriter(t *testing.T) {
	// First get a working MKVWriter
	var goodBuf seekBuffer
	m := NewMKVWriter(&goodBuf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true},
		},
	}
	if err := m.WriteMetadata(c, c.Tracks, 0); err != nil {
		t.Fatal(err)
	}

	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
	}
	if err := m.WriteClusterWithCues(0, 1000000, blocks); err != nil {
		t.Fatal(err)
	}

	// Test Finalize failing at various byte limits
	for extra := 0; extra < 100; extra++ {
		errBuf := &errSeekBuffer{
			buf:     append([]byte(nil), goodBuf.buf...),
			pos:     goodBuf.pos,
			written: goodBuf.pos,
			limit:   goodBuf.pos + extra,
		}
		m2 := *m
		m2.W = errBuf
		m2.Finalize() // errors expected
	}
}

func TestMKVWriterFinalizeSeekError(t *testing.T) {
	var goodBuf seekBuffer
	m := NewMKVWriter(&goodBuf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000}}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}

	m.W = &errOnSeekWriter{buf: goodBuf.buf, pos: goodBuf.pos}
	err := m.Finalize()
	if err == nil {
		t.Error("expected error from Seek failure")
	}
}

func TestMKVWriterFinalizeCuesWriteError(t *testing.T) {
	var goodBuf seekBuffer
	m := NewMKVWriter(&goodBuf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000}}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}

	// Add cues
	m.Cues = []mkv.CuePoint{
		{TimeMs: 0, Track: 1, ClusterPos: 100},
	}
	m.TimecodeScale = 1000000

	// Replace with errWriter that fails immediately
	errBuf := &errSeekBuffer{
		buf:     append([]byte(nil), goodBuf.buf...),
		pos:     goodBuf.pos,
		written: goodBuf.pos,
		limit:   goodBuf.pos, // fail immediately
	}
	m.W = errBuf
	err := m.Finalize()
	if err == nil {
		t.Error("expected error from WriteCues failure")
	}
}

func TestMKVWriterFinalizeWriteSeekDataError(t *testing.T) {
	var goodBuf seekBuffer
	m := NewMKVWriter(&goodBuf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1000000}}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}

	// Set up positions
	m.InfoPos = 100

	// Replace writer with one that succeeds on Seek but fails on Write
	m.W = &seekOkWriteFailWriter{
		buf: append([]byte(nil), goodBuf.buf...),
		pos: goodBuf.pos,
	}
	err := m.Finalize()
	if err == nil {
		t.Error("expected error from Write failure")
	}
}

type seekOkWriteFailWriter struct {
	buf []byte
	pos int
}

func (s *seekOkWriteFailWriter) Write(p []byte) (int, error) {
	return 0, io.ErrShortWrite
}

func (s *seekOkWriteFailWriter) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(s.pos) + offset
	case io.SeekEnd:
		abs = int64(len(s.buf)) + offset
	}
	s.pos = int(abs)
	return abs, nil
}

type errOnSeekWriter struct {
	buf []byte
	pos int
}

func (e *errOnSeekWriter) Write(p []byte) (int, error) {
	end := e.pos + len(p)
	if end > len(e.buf) {
		e.buf = append(e.buf, make([]byte, end-len(e.buf))...)
	}
	copy(e.buf[e.pos:], p)
	e.pos = end
	return len(p), nil
}

func (e *errOnSeekWriter) Seek(offset int64, whence int) (int64, error) {
	return 0, io.ErrNoProgress
}

func TestWriteMetadataWithErrSeekWriter(t *testing.T) {
	// First, determine how many bytes WriteStart writes
	var goodBuf seekBuffer
	m0 := NewMKVWriter(&goodBuf)
	if err := m0.WriteStart(); err != nil {
		t.Fatal(err)
	}
	startSize := len(goodBuf.buf)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true},
		},
		Chapters:    []mkv.Chapter{{ID: 1, Title: "Ch", StartMs: 0}},
		Attachments: []mkv.Attachment{{ID: 1, Name: "f", MIMEType: "a/b", Data: []byte{0x01}}},
		Tags:        []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "N", Value: "V"}}}},
	}

	// Now test WriteMetadata failing at each byte after WriteStart
	for extra := 0; extra < 500; extra++ {
		buf := &errSeekBuffer{limit: startSize + extra}
		m := NewMKVWriter(buf)
		if err := m.WriteStart(); err != nil {
			continue
		}
		m.WriteMetadata(c, c.Tracks, 5000) // errors expected
	}
}

func TestWriteEmptyContainer(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
	}
	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	// Should produce valid (minimal) output
	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "empty.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 0 {
		t.Errorf("tracks = %d, want 0", len(got.Tracks))
	}
}

func TestWriteWithOnlyChapters(t *testing.T) {
	c := &mkv.Container{
		Info:     mkv.SegmentInfo{TimecodeScale: 1000000},
		Chapters: []mkv.Chapter{{ID: 1, Title: "Ch", StartMs: 0, EndMs: 5000}},
	}
	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "chonly.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chapters) != 1 {
		t.Errorf("chapters = %d", len(got.Chapters))
	}
}

func TestWriteWithOnlyAttachments(t *testing.T) {
	c := &mkv.Container{
		Info:        mkv.SegmentInfo{TimecodeScale: 1000000},
		Attachments: []mkv.Attachment{{ID: 1, Name: "f", MIMEType: "a/b", Data: []byte{0x01}}},
	}
	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "attonly.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 {
		t.Errorf("attachments = %d", len(got.Attachments))
	}
}

func TestWriteWithOnlyTags(t *testing.T) {
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tags: []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Test"}}}},
	}
	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(buf.Bytes())
	got, err := reader.Read(context.Background(), r, "tagonly.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tags) != 1 {
		t.Errorf("tags = %d", len(got.Tags))
	}
}

func TestWriteClusterWithErrBreaksLoop(t *testing.T) {
	// Test WriteCluster when block writing fails mid-loop
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 1, Timecode: 33, Keyframe: false, Data: []byte{0x02}},
		{TrackNumber: 1, Timecode: 66, Keyframe: false, Data: []byte{0x03}},
	}
	// Try various error positions to hit the e.err break in WriteCluster loop
	for limit := 1; limit < 50; limit++ {
		ew := &errWriter{limit: limit}
		WriteCluster(ew, 0, 1000000, blocks)
	}
}

func TestFinalizeSeekHeadOverflowWritesFallback(t *testing.T) {
	// Make a seek head that exceeds SeekHeadReserve
	var buf seekBuffer
	m := NewMKVWriter(&buf)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	// Write many clusters to generate many cues, making seek head potentially large
	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
	}
	if err := m.WriteMetadata(c, nil, 0); err != nil {
		t.Fatal(err)
	}
	// Manually set positions to create a valid seek head
	m.InfoPos = 10
	m.TracksPos = 20
	m.ChaptersPos = 30
	m.AttachPos = 40
	m.TagsPos = 50
	m.CuesPos = 60

	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
}

// errSeekBuffer is a seekable writer that fails after limit bytes
type errSeekBuffer struct {
	buf     []byte
	pos     int
	written int
	limit   int
}

func (s *errSeekBuffer) Write(p []byte) (int, error) {
	remaining := s.limit - s.written
	if remaining <= 0 {
		return 0, io.ErrShortWrite
	}
	n := len(p)
	if n > remaining {
		n = remaining
	}
	end := s.pos + n
	if end > len(s.buf) {
		s.buf = append(s.buf, make([]byte, end-len(s.buf))...)
	}
	copy(s.buf[s.pos:], p[:n])
	s.pos = end
	s.written += n
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func (s *errSeekBuffer) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(s.pos) + offset
	case io.SeekEnd:
		abs = int64(len(s.buf)) + offset
	}
	if abs < 0 {
		abs = 0
	}
	s.pos = int(abs)
	return abs, nil
}

// errWriter fails after writing `limit` bytes
type errWriter struct {
	written int
	limit   int
}

func (e *errWriter) Write(p []byte) (int, error) {
	remaining := e.limit - e.written
	if remaining <= 0 {
		return 0, io.ErrShortWrite
	}
	if len(p) > remaining {
		e.written += remaining
		return remaining, io.ErrShortWrite
	}
	e.written += len(p)
	return len(p), nil
}

// seekBuffer is a bytes.Buffer that implements io.WriteSeeker.
type seekBuffer struct {
	buf []byte
	pos int
}

func (s *seekBuffer) Write(p []byte) (int, error) {
	end := s.pos + len(p)
	if end > len(s.buf) {
		s.buf = append(s.buf, make([]byte, end-len(s.buf))...)
	}
	copy(s.buf[s.pos:], p)
	s.pos = end
	return len(p), nil
}

func (s *seekBuffer) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(s.pos) + offset
	case io.SeekEnd:
		abs = int64(len(s.buf)) + offset
	}
	if abs < 0 {
		abs = 0
	}
	s.pos = int(abs)
	return abs, nil
}
