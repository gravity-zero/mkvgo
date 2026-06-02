package reader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func writeEBMLHeader(buf *bytes.Buffer) {
	ebml.WriteElementHeader(buf, ebml.IDEBMLHeader, 0)
}

func writeSegmentStart(buf *bytes.Buffer, size int64) {
	ebml.WriteElementHeader(buf, mkv.IDSegment, size)
}

func buildMinimalMKV() *bytes.Buffer {
	var inner bytes.Buffer
	ebml.WriteElementHeader(&inner, mkv.IDInfo, 0)

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(inner.Len()))
	buf.Write(inner.Bytes())
	return &buf
}

func TestTruncatedEBMLHeader(t *testing.T) {
	// single byte — not enough for a full EBML header
	r := bytes.NewReader([]byte{0x1A})
	_, err := Read(context.Background(), r, "trunc.mkv")
	if err == nil {
		t.Fatal("expected error for truncated EBML header")
	}
}

func TestEmptyFile(t *testing.T) {
	r := bytes.NewReader(nil)
	_, err := Read(context.Background(), r, "empty.mkv")
	if err == nil {
		t.Fatal("expected error for empty file")
	}
}

func TestInvalidVINT(t *testing.T) {
	// 0x00 is invalid as a VINT leading byte
	r := bytes.NewReader([]byte{0x00})
	_, err := Read(context.Background(), r, "bad.mkv")
	if err == nil {
		t.Fatal("expected error for zero VINT byte")
	}
	if !strings.Contains(err.Error(), "VINT") && !strings.Contains(err.Error(), "zero") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// realCluster builds a minimal but realistic Cluster element: it opens with a
// Timestamp (the first child the resync validator looks for) and carries one
// tiny SimpleBlock, so isClusterAt accepts it as a genuine cluster.
func realCluster() []byte {
	var body bytes.Buffer
	ebml.WriteElementHeader(&body, mkv.IDTimestamp, 1)
	body.WriteByte(0x00)
	var sb bytes.Buffer
	ebml.WriteDataSize(&sb, 1)               // track number VINT = 1
	sb.Write([]byte{0x00, 0x00, 0x80, 0xAA}) // relTC=0, flags=keyframe, 1 data byte
	ebml.WriteElementHeader(&body, mkv.IDSimpleBlock, int64(sb.Len()))
	body.Write(sb.Bytes())

	var clu bytes.Buffer
	ebml.WriteElementHeader(&clu, mkv.IDCluster, int64(body.Len()))
	clu.Write(body.Bytes())
	return clu.Bytes()
}

func gappedInfo() []byte {
	var info bytes.Buffer
	var tcs bytes.Buffer
	ebml.WriteUint(&tcs, 500000, 4) // non-default TimecodeScale
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, int64(tcs.Len()))
	info.Write(tcs.Bytes())
	var dur bytes.Buffer
	ebml.WriteFloat(&dur, 1234)
	ebml.WriteElementHeader(&info, mkv.IDDuration, int64(dur.Len()))
	info.Write(dur.Bytes())

	var out bytes.Buffer
	ebml.WriteElementHeader(&out, mkv.IDInfo, int64(info.Len()))
	out.Write(info.Bytes())
	return out.Bytes()
}

func wrapSegment(inner []byte) []byte {
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(len(inner)))
	buf.Write(inner)
	return buf.Bytes()
}

func oneCuePoint() []byte {
	var cue bytes.Buffer
	ebml.WriteElementHeader(&cue, mkv.IDCuePoint, 0)
	var out bytes.Buffer
	ebml.WriteElementHeader(&out, mkv.IDCues, int64(cue.Len()))
	out.Write(cue.Bytes())
	return out.Bytes()
}

// buildGappedMKV builds a valid MKV whose body contains a run of zero bytes
// between two clusters — the corruption pattern seen in some real-world rips,
// where a multi-MB region is zeroed out. If postCluster is true a recoverable
// Cluster (followed by a Cues element) is placed after the gap.
func buildGappedMKV(gap int, postCluster bool) []byte {
	var inner bytes.Buffer
	inner.Write(gappedInfo())
	inner.Write(realCluster())     // first (good) cluster
	inner.Write(make([]byte, gap)) // zeroed/corrupted region
	if postCluster {
		inner.Write(realCluster()) // resync anchor
		inner.Write(oneCuePoint())
	}
	return wrapSegment(inner.Bytes())
}

func TestRecoverFromZeroPaddingGap(t *testing.T) {
	tests := []struct {
		name        string
		gap         int
		postCluster bool
		wantCues    int
	}{
		// gap > 64 KiB resync window: exercises the boundary-carry path.
		{"recoverable gap", 200 << 10, true, 1},
		// corruption runs to the segment end: nothing to resync to, but the
		// metadata read before the gap must still succeed (no error).
		{"gap to end", 4096, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildGappedMKV(tt.gap, tt.postCluster)
			c, err := Read(context.Background(), bytes.NewReader(data), tt.name+".mkv")
			if err != nil {
				t.Fatalf("Read returned error on recoverable corruption: %v", err)
			}
			if c.Info.TimecodeScale != 500000 {
				t.Errorf("pre-gap Info lost: TimecodeScale = %d, want 500000", c.Info.TimecodeScale)
			}
			if c.Info.Duration != 1234 {
				t.Errorf("pre-gap Info lost: Duration = %g, want 1234", c.Info.Duration)
			}
			if len(c.Cues) != tt.wantCues {
				t.Errorf("post-gap recovery: got %d cues, want %d", len(c.Cues), tt.wantCues)
			}
		})
	}
}

// TestResyncSkipsFalseClusterMagic plants the 4-byte Cluster ID by chance inside
// the corrupted region. The false anchor declares a huge size: a naive resync
// that trusted it would skip clean past the genuine cluster AND the Cues (losing
// them), so this test fails if the validation in isClusterAt is removed. With
// the guard, the candidate is rejected (its declared size overruns the segment)
// and recovery lands on the real cluster, so the Cues survive.
func TestResyncSkipsFalseClusterMagic(t *testing.T) {
	// Cluster magic + a 3-byte size VINT encoding 65535 — far past segment end.
	falseAnchor := []byte{0x1F, 0x43, 0xB6, 0x75, 0x20, 0xFF, 0xFF}

	var gap bytes.Buffer
	gap.Write(make([]byte, 4096))
	gap.Write(falseAnchor)
	gap.Write(make([]byte, 4096))

	var inner bytes.Buffer
	inner.Write(gappedInfo())
	inner.Write(realCluster()) // first good cluster
	inner.Write(gap.Bytes())   // corruption containing a false magic
	inner.Write(realCluster()) // the genuine resync target
	inner.Write(oneCuePoint())

	data := wrapSegment(inner.Bytes())
	c, err := Read(context.Background(), bytes.NewReader(data), "false-magic.mkv")
	if err != nil {
		t.Fatalf("Read errored instead of skipping the false magic: %v", err)
	}
	if len(c.Cues) != 1 {
		t.Fatalf("expected recovery at the genuine cluster (1 cue), got %d — false magic was likely trusted", len(c.Cues))
	}
}

// TestRecoveryMultipleGaps deliberately corrupts the body with TWO separate
// zero-padding regions; the parser must resync past both and still reach the
// trailing Cues.
func TestRecoveryMultipleGaps(t *testing.T) {
	var inner bytes.Buffer
	inner.Write(gappedInfo())
	inner.Write(realCluster())
	inner.Write(make([]byte, 70<<10)) // gap 1 (> 64 KiB window)
	inner.Write(realCluster())
	inner.Write(make([]byte, 70<<10)) // gap 2
	inner.Write(realCluster())
	inner.Write(oneCuePoint())

	c, err := Read(context.Background(), bytes.NewReader(wrapSegment(inner.Bytes())), "multigap.mkv")
	if err != nil {
		t.Fatalf("Read across 2 gaps: %v", err)
	}
	if c.Info.TimecodeScale != 500000 {
		t.Errorf("pre-corruption Info lost: %d", c.Info.TimecodeScale)
	}
	if len(c.Cues) != 1 {
		t.Errorf("recovery across 2 gaps failed: got %d cues, want 1", len(c.Cues))
	}
}

// TestRecoveryTruncatedInGap simulates a truncated download: the Segment claims
// more bytes than the file actually holds, and the data ends inside a corrupted
// gap. The parser must return the metadata it already gathered, without erroring
// or panicking.
func TestRecoveryTruncatedInGap(t *testing.T) {
	var inner bytes.Buffer
	inner.Write(gappedInfo())
	inner.Write(realCluster())
	inner.Write(make([]byte, 8192)) // gap — then the file just stops here

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(inner.Len())+100000) // Segment claims more than exists
	buf.Write(inner.Bytes())

	c, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "truncated.mkv")
	if err != nil {
		t.Fatalf("truncated file should recover gracefully, got: %v", err)
	}
	if c.Info.TimecodeScale != 500000 {
		t.Errorf("pre-truncation Info lost: %d", c.Info.TimecodeScale)
	}
}

// TestReadCorruptionNoPanic feeds many deliberately-corrupted variants of a real
// muxer fixture (truncations, zeroed regions, garbage injection, header
// byte-flips) to Read. The contract under any corruption: never panic — only
// return data or an error. (A real-corpus complement to FuzzRead.)
func TestReadCorruptionNoPanic(t *testing.T) {
	orig, err := os.ReadFile("../../internal/testdata/sample.mkv")
	if err != nil {
		t.Skipf("sample.mkv not available: %v", err)
	}
	n := len(orig)

	read := func(label string, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on %s: %v", label, r)
			}
		}()
		_, _ = Read(context.Background(), bytes.NewReader(data), "corrupt.mkv")
	}

	for _, p := range []int{1, 5, 10, 25, 50, 75, 90, 99} {
		read(fmt.Sprintf("trunc-%d%%", p), orig[:n*p/100])
	}
	for _, p := range []int{5, 20, 40, 60, 80} {
		d := append([]byte(nil), orig...)
		for i := n * p / 100; i < n*p/100+4096 && i < n; i++ {
			d[i] = 0
		}
		read(fmt.Sprintf("zerogap-%d%%", p), d)
	}
	for _, p := range []int{5, 30, 60, 90} {
		d := append([]byte(nil), orig...)
		for i := n * p / 100; i < n*p/100+512 && i < n; i++ {
			d[i] = 0xFF
		}
		read(fmt.Sprintf("garbage-%d%%", p), d)
	}
	for off := 0; off < 2048 && off < n; off += 8 {
		d := append([]byte(nil), orig...)
		d[off] ^= 0xFF
		read(fmt.Sprintf("flip@%d", off), d)
	}
}

func TestHugeElementSize(t *testing.T) {
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	// Segment with huge inner element
	var inner bytes.Buffer
	// write an Info element claiming size > MaxElementSize
	ebml.WriteElementID(&inner, mkv.IDTitle)
	// encode a huge data size manually: 8-byte VINT for ~1TB
	inner.Write([]byte{0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00})

	writeSegmentStart(&buf, int64(inner.Len())+100)
	ebml.WriteElementHeader(&buf, mkv.IDInfo, int64(inner.Len()))
	buf.Write(inner.Bytes())

	r := bytes.NewReader(buf.Bytes())
	_, err := Read(context.Background(), r, "huge.mkv")
	if err == nil {
		t.Fatal("expected error for huge element size")
	}
}

func TestTagRecursionDepthLimit(t *testing.T) {
	// Build deeply nested SimpleTags beyond maxTagDepth (64)
	const depth = 70
	var innermost bytes.Buffer
	ebml.WriteElementHeader(&innermost, mkv.IDTagName, 3)
	ebml.WriteString(&innermost, "end")

	payload := innermost.Bytes()
	for i := 0; i < depth; i++ {
		var wrap bytes.Buffer
		ebml.WriteElementHeader(&wrap, mkv.IDSimpleTag, int64(len(payload)))
		wrap.Write(payload)
		payload = wrap.Bytes()
	}

	// Wrap in Tag > Targets + SimpleTags
	var tag bytes.Buffer
	ebml.WriteElementHeader(&tag, mkv.IDTargets, 0)
	tag.Write(payload)

	var tags bytes.Buffer
	ebml.WriteElementHeader(&tags, mkv.IDTag, int64(tag.Len()))
	tags.Write(tag.Bytes())

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	ebml.WriteElementHeader(&seg, mkv.IDTags, int64(tags.Len()))
	seg.Write(tags.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	_, err := Read(context.Background(), r, "deep.mkv")
	if err == nil {
		t.Fatal("expected error for deep SimpleTag nesting")
	}
	if !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNestedChapterAtom(t *testing.T) {
	// Build a chapter with a sub-chapter
	var subDisplay bytes.Buffer
	ebml.WriteElementHeader(&subDisplay, mkv.IDChapString, 3)
	ebml.WriteString(&subDisplay, "Sub")

	var subAtom bytes.Buffer
	ebml.WriteElementHeader(&subAtom, mkv.IDChapterUID, 1)
	ebml.WriteUint(&subAtom, 2, 1)
	ebml.WriteElementHeader(&subAtom, mkv.IDChapterTimeStart, 1)
	ebml.WriteUint(&subAtom, 0, 1)
	ebml.WriteElementHeader(&subAtom, mkv.IDChapterDisplay, int64(subDisplay.Len()))
	subAtom.Write(subDisplay.Bytes())

	var parentDisplay bytes.Buffer
	ebml.WriteElementHeader(&parentDisplay, mkv.IDChapString, 6)
	ebml.WriteString(&parentDisplay, "Parent")

	var parentAtom bytes.Buffer
	ebml.WriteElementHeader(&parentAtom, mkv.IDChapterUID, 1)
	ebml.WriteUint(&parentAtom, 1, 1)
	ebml.WriteElementHeader(&parentAtom, mkv.IDChapterTimeStart, 1)
	ebml.WriteUint(&parentAtom, 0, 1)
	ebml.WriteElementHeader(&parentAtom, mkv.IDChapterDisplay, int64(parentDisplay.Len()))
	parentAtom.Write(parentDisplay.Bytes())
	ebml.WriteElementHeader(&parentAtom, mkv.IDChapterAtom, int64(subAtom.Len()))
	parentAtom.Write(subAtom.Bytes())

	var edition bytes.Buffer
	ebml.WriteElementHeader(&edition, mkv.IDChapterAtom, int64(parentAtom.Len()))
	edition.Write(parentAtom.Bytes())

	var chapters bytes.Buffer
	ebml.WriteElementHeader(&chapters, mkv.IDEditionEntry, int64(edition.Len()))
	chapters.Write(edition.Bytes())

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	ebml.WriteElementHeader(&seg, mkv.IDChapters, int64(chapters.Len()))
	seg.Write(chapters.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "nested.mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.Chapters) != 1 {
		t.Fatalf("expected 1 chapter, got %d", len(c.Chapters))
	}
	if c.Chapters[0].Title != "Parent" {
		t.Errorf("parent title = %q, want %q", c.Chapters[0].Title, "Parent")
	}
	if len(c.Chapters[0].SubChapters) != 1 {
		t.Fatalf("expected 1 sub-chapter, got %d", len(c.Chapters[0].SubChapters))
	}
	if c.Chapters[0].SubChapters[0].Title != "Sub" {
		t.Errorf("sub title = %q, want %q", c.Chapters[0].SubChapters[0].Title, "Sub")
	}
}

func TestContentEncodingsRoundTrip(t *testing.T) {
	headerBytes := []byte{0x01, 0x02, 0x03, 0x04}
	w := uint32(1920)
	h := uint32(1080)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, MuxingApp: "test", WritingApp: "test"},
		Tracks: []mkv.Track{
			{
				ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng",
				IsDefault: true, Width: &w, Height: &h,
				HeaderStripping: headerBytes,
			},
		},
	}

	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := bytes.NewReader(buf.Bytes())
	got, err := Read(context.Background(), r, "test.mkv")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if len(got.Tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(got.Tracks))
	}
	track := got.Tracks[0]
	if !bytes.Equal(track.HeaderStripping, headerBytes) {
		t.Errorf("HeaderStripping = %x, want %x", track.HeaderStripping, headerBytes)
	}
}

func TestContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	buf := buildMinimalMKV()
	r := bytes.NewReader(buf.Bytes())
	_, err := Read(ctx, r, "cancel.mkv")
	if err == nil {
		t.Fatal("expected context cancelled error")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	w := uint32(640)
	h := uint32(480)
	sr := 48000.0
	ch := uint8(2)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{
			TimecodeScale: 1000000,
			Title:         "Test",
			MuxingApp:     "mkvgo",
			WritingApp:    "mkvgo",
		},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "opus", Language: "fre", IsDefault: false, IsForced: true, SampleRate: &sr, Channels: &ch},
			{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "eng", Name: "English"},
		},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Chapter 1", StartMs: 0, EndMs: 5000},
		},
		Tags: []mkv.Tag{
			{TargetType: "MOVIE", TargetID: 0, SimpleTags: []mkv.SimpleTag{
				{Name: "TITLE", Value: "Test Movie", Language: "eng"},
			}},
		},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "font.ttf", MIMEType: "font/ttf", Data: []byte{0x00, 0x01}},
		},
		DurationMs: 10000,
	}

	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := bytes.NewReader(buf.Bytes())
	got, err := Read(context.Background(), r, "roundtrip.mkv")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if got.Info.Title != "Test" {
		t.Errorf("title = %q, want %q", got.Info.Title, "Test")
	}
	if len(got.Tracks) != 3 {
		t.Fatalf("tracks = %d, want 3", len(got.Tracks))
	}
	if got.Tracks[0].Codec != "h264" {
		t.Errorf("track 0 codec = %q, want h264", got.Tracks[0].Codec)
	}
	if got.Tracks[1].IsForced != true {
		t.Error("track 1 should be forced")
	}
	if got.Tracks[1].IsDefault != false {
		t.Error("track 1 should not be default")
	}
	if got.Tracks[2].Name != "English" {
		t.Errorf("track 2 name = %q, want English", got.Tracks[2].Name)
	}
	if len(got.Chapters) != 1 {
		t.Fatalf("chapters = %d, want 1", len(got.Chapters))
	}
	if got.Chapters[0].Title != "Chapter 1" {
		t.Errorf("chapter title = %q", got.Chapters[0].Title)
	}
	if len(got.Tags) != 1 || len(got.Tags[0].SimpleTags) != 1 {
		t.Fatalf("tags mismatch")
	}
	if got.Tags[0].SimpleTags[0].Name != "TITLE" {
		t.Errorf("tag name = %q", got.Tags[0].SimpleTags[0].Name)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(got.Attachments))
	}
	if got.Attachments[0].Name != "font.ttf" {
		t.Errorf("attachment name = %q", got.Attachments[0].Name)
	}
}

func TestWrongEBMLHeaderID(t *testing.T) {
	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, mkv.IDSegment, 0) // wrong: Segment instead of EBML header
	r := bytes.NewReader(buf.Bytes())
	_, err := Read(context.Background(), r, "wrong.mkv")
	if err == nil {
		t.Fatal("expected error for wrong EBML header ID")
	}
	if !strings.Contains(err.Error(), "expected EBML") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestWrongSegmentID(t *testing.T) {
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	ebml.WriteElementHeader(&buf, mkv.IDInfo, 0) // wrong: Info instead of Segment
	r := bytes.NewReader(buf.Bytes())
	_, err := Read(context.Background(), r, "wrong.mkv")
	if err == nil {
		t.Fatal("expected error for wrong segment ID")
	}
	if !strings.Contains(err.Error(), "expected Segment") {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestCuesRoundTrip(t *testing.T) {
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)

	var cuesBuf bytes.Buffer
	err := writer.WriteCues(&cuesBuf, []mkv.CuePoint{
		{TimeMs: 0, Track: 1, ClusterPos: 100},
		{TimeMs: 5000, Track: 1, ClusterPos: 5000},
	}, 1000000)
	if err != nil {
		t.Fatal(err)
	}
	seg.Write(cuesBuf.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "cues.mkv")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(c.Cues) != 2 {
		t.Fatalf("cues = %d, want 2", len(c.Cues))
	}
	if c.Cues[1].ClusterPos != 5000 {
		t.Errorf("cue 1 pos = %d, want 5000", c.Cues[1].ClusterPos)
	}
}

func TestOpenSampleMKV(t *testing.T) {
	path := "../../internal/testdata/sample.mkv"
	c, err := Open(context.Background(), path)
	if err != nil {
		t.Skipf("sample.mkv not available: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Error("expected at least one track")
	}
}

func TestOpenNonexistent(t *testing.T) {
	_, err := Open(context.Background(), "/nonexistent/file.mkv")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestOpenWithFSNonexistent(t *testing.T) {
	_, err := OpenWithFS(context.Background(), "/nonexistent/file.mkv", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestSegmentInfoWithAllFields(t *testing.T) {
	var seg bytes.Buffer

	var info bytes.Buffer
	ts := int64(1000000)
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, int64(ebml.UintLen(uint64(ts))))
	ebml.WriteUint(&info, uint64(ts), ebml.UintLen(uint64(ts)))
	ebml.WriteElementHeader(&info, mkv.IDDuration, 8)
	ebml.WriteFloat(&info, 10000.0)
	ebml.WriteElementHeader(&info, mkv.IDTitle, 4)
	ebml.WriteString(&info, "Test")
	ebml.WriteElementHeader(&info, mkv.IDMuxingApp, 5)
	ebml.WriteString(&info, "mkvgo")
	ebml.WriteElementHeader(&info, mkv.IDWritingApp, 5)
	ebml.WriteString(&info, "mkvgo")
	// DateUTC
	ebml.WriteElementHeader(&info, mkv.IDDateUTC, 8)
	ebml.WriteUint(&info, 0, 8)
	// SegmentUID
	uid := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	ebml.WriteElementHeader(&info, mkv.IDSegmentUID, int64(len(uid)))
	ebml.WriteBytes(&info, uid)
	// PrevUID
	ebml.WriteElementHeader(&info, mkv.IDPrevUID, int64(len(uid)))
	ebml.WriteBytes(&info, uid)
	// NextUID
	ebml.WriteElementHeader(&info, mkv.IDNextUID, int64(len(uid)))
	ebml.WriteBytes(&info, uid)

	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "info.mkv")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if c.Info.Title != "Test" {
		t.Errorf("title = %q", c.Info.Title)
	}
	if c.Info.DateUTC == nil {
		t.Error("DateUTC should be set")
	}
	if len(c.Info.SegmentUID) != 16 {
		t.Errorf("SegmentUID len = %d", len(c.Info.SegmentUID))
	}
	if len(c.Info.PrevUID) != 16 {
		t.Errorf("PrevUID len = %d", len(c.Info.PrevUID))
	}
	if len(c.Info.NextUID) != 16 {
		t.Errorf("NextUID len = %d", len(c.Info.NextUID))
	}
}

func TestAudioTrackFields(t *testing.T) {
	sr := 44100.0
	ch := uint8(6)
	bd := uint8(24)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.AudioTrack, Codec: "flac", Language: "eng", IsDefault: true,
				SampleRate: &sr, Channels: &ch, BitDepth: &bd},
		},
	}

	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(buf.Bytes())
	got, err := Read(context.Background(), r, "audio.mkv")
	if err != nil {
		t.Fatal(err)
	}
	track := got.Tracks[0]
	if track.SampleRate == nil || *track.SampleRate != 44100.0 {
		t.Errorf("SampleRate = %v", track.SampleRate)
	}
	if track.Channels == nil || *track.Channels != 6 {
		t.Errorf("Channels = %v", track.Channels)
	}
	if track.BitDepth == nil || *track.BitDepth != 24 {
		t.Errorf("BitDepth = %v", track.BitDepth)
	}
}

func TestEditionFlagOrdered(t *testing.T) {
	var edition bytes.Buffer
	ebml.WriteElementHeader(&edition, mkv.IDEditionFlagOrdered, 1)
	ebml.WriteUint(&edition, 1, 1)

	var atom bytes.Buffer
	ebml.WriteElementHeader(&atom, mkv.IDChapterUID, 1)
	ebml.WriteUint(&atom, 1, 1)
	ebml.WriteElementHeader(&atom, mkv.IDChapterTimeStart, 1)
	ebml.WriteUint(&atom, 0, 1)
	ebml.WriteElementHeader(&atom, mkv.IDChapterTimeEnd, 2)
	ebml.WriteUint(&atom, 5000000, 2)
	// ChapterSegmentUID
	segUID := []byte{0xAA, 0xBB}
	ebml.WriteElementHeader(&atom, mkv.IDChapterSegmentUID, int64(len(segUID)))
	ebml.WriteBytes(&atom, segUID)

	ebml.WriteElementHeader(&edition, mkv.IDChapterAtom, int64(atom.Len()))
	edition.Write(atom.Bytes())

	var chapters bytes.Buffer
	ebml.WriteElementHeader(&chapters, mkv.IDEditionEntry, int64(edition.Len()))
	chapters.Write(edition.Bytes())

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	ebml.WriteElementHeader(&seg, mkv.IDChapters, int64(chapters.Len()))
	seg.Write(chapters.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "ordered.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Chapters) != 1 {
		t.Fatalf("chapters = %d", len(c.Chapters))
	}
	if !bytes.Equal(c.Chapters[0].SegmentUID, segUID) {
		t.Errorf("SegmentUID = %x", c.Chapters[0].SegmentUID)
	}
}

func TestTagWithTargetID(t *testing.T) {
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)

	tags := []mkv.Tag{{
		TargetType: "EPISODE",
		TargetID:   42,
		SimpleTags: []mkv.SimpleTag{
			{Name: "TITLE", Value: "ep1", Language: "eng"},
		},
	}}
	var tagsBuf bytes.Buffer
	if err := writer.WriteTags(&tagsBuf, tags); err != nil {
		t.Fatal(err)
	}
	seg.Write(tagsBuf.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "tags.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tags) != 1 {
		t.Fatal("expected 1 tag")
	}
	if c.Tags[0].TargetID != 42 {
		t.Errorf("TargetID = %d, want 42", c.Tags[0].TargetID)
	}
	if c.Tags[0].TargetType != "EPISODE" {
		t.Errorf("TargetType = %q", c.Tags[0].TargetType)
	}
	if c.Tags[0].SimpleTags[0].Language != "eng" {
		t.Errorf("Language = %q", c.Tags[0].SimpleTags[0].Language)
	}
}

func TestReadDurationOverflow(t *testing.T) {
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 8)
	ebml.WriteUint(&info, uint64(1<<62), 8)
	ebml.WriteElementHeader(&info, mkv.IDDuration, 8)
	ebml.WriteFloat(&info, 1e30)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	_, err := Read(context.Background(), r, "overflow.mkv")
	if err == nil {
		t.Fatal("expected duration overflow error")
	}
}

func TestOpenWithFSSuccess(t *testing.T) {
	path := "../../internal/testdata/sample.mkv"
	c, err := OpenWithFS(context.Background(), path, nil)
	if err != nil {
		t.Skipf("sample.mkv not available: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Error("expected at least one track")
	}
}

func TestTagWithBinaryAndSubTags(t *testing.T) {
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)

	tags := []mkv.Tag{{
		TargetType: "MOVIE",
		SimpleTags: []mkv.SimpleTag{
			{Name: "COVER", Binary: []byte{0xFF, 0xD8}, SubTags: []mkv.SimpleTag{
				{Name: "NESTED", Value: "val"},
			}},
		},
	}}
	var tagsBuf bytes.Buffer
	if err := writer.WriteTags(&tagsBuf, tags); err != nil {
		t.Fatal(err)
	}
	seg.Write(tagsBuf.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "tags.mkv")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(c.Tags) != 1 || len(c.Tags[0].SimpleTags) != 1 {
		t.Fatal("tag structure mismatch")
	}
	st := c.Tags[0].SimpleTags[0]
	if !bytes.Equal(st.Binary, []byte{0xFF, 0xD8}) {
		t.Errorf("binary = %x", st.Binary)
	}
	if len(st.SubTags) != 1 || st.SubTags[0].Name != "NESTED" {
		t.Error("subtags mismatch")
	}
}

// truncReader truncates reads after limit bytes, producing errors in parsers
type truncReader struct {
	data []byte
	pos  int
}

func (r *truncReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *truncReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = int(offset)
	case io.SeekCurrent:
		r.pos += int(offset)
	case io.SeekEnd:
		r.pos = len(r.data) + int(offset)
	}
	if r.pos < 0 {
		r.pos = 0
	}
	if r.pos > len(r.data) {
		r.pos = len(r.data)
	}
	return int64(r.pos), nil
}

func TestUnknownElementsInParsers(t *testing.T) {
	// Craft MKV with unknown element IDs inside various master elements
	// to exercise the default: skip branches in all parsers.
	// Use 0x42F7 (a valid EBML ID not used in Matroska) as "unknown".
	unknownID := uint32(0x42F7)

	// Helper: write an unknown element with 2 bytes of data
	writeUnknown := func(buf *bytes.Buffer) {
		ebml.WriteElementHeader(buf, unknownID, 2)
		buf.Write([]byte{0xAB, 0xCD})
	}

	// Info with unknown element
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 1000000, 3)
	writeUnknown(&info)

	// Track with unknown element + Video with unknown + Audio with unknown
	w := uint32(640)
	h := uint32(480)
	sr := 48000.0
	ch := uint8(2)

	// Video sub with unknown
	var videoSub bytes.Buffer
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelWidth, 2)
	ebml.WriteUint(&videoSub, uint64(w), 2)
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelHeight, 2)
	ebml.WriteUint(&videoSub, uint64(h), 2)
	writeUnknown(&videoSub)

	// Audio sub with unknown
	var audioSub bytes.Buffer
	ebml.WriteElementHeader(&audioSub, mkv.IDSamplingFreq, 8)
	ebml.WriteFloat(&audioSub, sr)
	ebml.WriteElementHeader(&audioSub, mkv.IDChannels, 1)
	ebml.WriteUint(&audioSub, uint64(ch), 1)
	writeUnknown(&audioSub)

	// ContentCompression with unknown
	var compSub bytes.Buffer
	ebml.WriteElementHeader(&compSub, mkv.IDContentCompSettings, 2)
	ebml.WriteBytes(&compSub, []byte{0x01, 0x02})
	writeUnknown(&compSub)

	// ContentEncoding with unknown + compression
	var encSub bytes.Buffer
	ebml.WriteElementHeader(&encSub, mkv.IDContentCompression, int64(compSub.Len()))
	encSub.Write(compSub.Bytes())
	writeUnknown(&encSub)

	// ContentEncodings with unknown + encoding
	var encsSub bytes.Buffer
	ebml.WriteElementHeader(&encsSub, mkv.IDContentEncoding, int64(encSub.Len()))
	encsSub.Write(encSub.Bytes())
	writeUnknown(&encsSub)

	// Track entry with video, audio-ish and unknowns
	var trackV bytes.Buffer
	ebml.WriteElementHeader(&trackV, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&trackV, 1, 1)
	ebml.WriteElementHeader(&trackV, mkv.IDTrackType, 1)
	ebml.WriteUint(&trackV, mkv.TrackTypeVideo, 1)
	ebml.WriteElementHeader(&trackV, mkv.IDCodecID, 14)
	ebml.WriteString(&trackV, "V_MPEG4/ISO/AVC")
	ebml.WriteElementHeader(&trackV, mkv.IDVideo, int64(videoSub.Len()))
	trackV.Write(videoSub.Bytes())
	ebml.WriteElementHeader(&trackV, mkv.IDContentEncodings, int64(encsSub.Len()))
	trackV.Write(encsSub.Bytes())
	writeUnknown(&trackV) // unknown in track entry

	var trackA bytes.Buffer
	ebml.WriteElementHeader(&trackA, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&trackA, 2, 1)
	ebml.WriteElementHeader(&trackA, mkv.IDTrackType, 1)
	ebml.WriteUint(&trackA, mkv.TrackTypeAudio, 1)
	ebml.WriteElementHeader(&trackA, mkv.IDCodecID, 6)
	ebml.WriteString(&trackA, "A_OPUS")
	ebml.WriteElementHeader(&trackA, mkv.IDAudio, int64(audioSub.Len()))
	trackA.Write(audioSub.Bytes())

	var tracks bytes.Buffer
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(trackV.Len()))
	tracks.Write(trackV.Bytes())
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(trackA.Len()))
	tracks.Write(trackA.Bytes())
	writeUnknown(&tracks) // unknown in Tracks

	// Chapters with unknowns
	var chapDisplay bytes.Buffer
	ebml.WriteElementHeader(&chapDisplay, mkv.IDChapString, 3)
	ebml.WriteString(&chapDisplay, "Ch1")
	writeUnknown(&chapDisplay)

	var chapAtom bytes.Buffer
	ebml.WriteElementHeader(&chapAtom, mkv.IDChapterUID, 1)
	ebml.WriteUint(&chapAtom, 1, 1)
	ebml.WriteElementHeader(&chapAtom, mkv.IDChapterTimeStart, 1)
	ebml.WriteUint(&chapAtom, 0, 1)
	ebml.WriteElementHeader(&chapAtom, mkv.IDChapterDisplay, int64(chapDisplay.Len()))
	chapAtom.Write(chapDisplay.Bytes())
	writeUnknown(&chapAtom) // unknown in ChapterAtom

	var edition bytes.Buffer
	ebml.WriteElementHeader(&edition, mkv.IDChapterAtom, int64(chapAtom.Len()))
	edition.Write(chapAtom.Bytes())
	writeUnknown(&edition) // unknown in EditionEntry

	var chapters bytes.Buffer
	ebml.WriteElementHeader(&chapters, mkv.IDEditionEntry, int64(edition.Len()))
	chapters.Write(edition.Bytes())
	writeUnknown(&chapters) // unknown in Chapters

	// Attachments with unknown
	var attFile bytes.Buffer
	ebml.WriteElementHeader(&attFile, mkv.IDFileUID, 1)
	ebml.WriteUint(&attFile, 1, 1)
	ebml.WriteElementHeader(&attFile, mkv.IDFileName, 5)
	ebml.WriteString(&attFile, "f.ttf")
	ebml.WriteElementHeader(&attFile, mkv.IDFileMimeType, 8)
	ebml.WriteString(&attFile, "font/ttf")
	ebml.WriteElementHeader(&attFile, mkv.IDFileData, 2)
	ebml.WriteBytes(&attFile, []byte{0x01, 0x02})
	writeUnknown(&attFile) // unknown in AttachedFile

	var attachments bytes.Buffer
	ebml.WriteElementHeader(&attachments, mkv.IDAttachedFile, int64(attFile.Len()))
	attachments.Write(attFile.Bytes())
	writeUnknown(&attachments) // unknown in Attachments

	// Tags with unknowns
	var targets bytes.Buffer
	ebml.WriteElementHeader(&targets, mkv.IDTargetType, 5)
	ebml.WriteString(&targets, "MOVIE")
	writeUnknown(&targets) // unknown in Targets

	var simpleTag bytes.Buffer
	ebml.WriteElementHeader(&simpleTag, mkv.IDTagName, 5)
	ebml.WriteString(&simpleTag, "TITLE")
	ebml.WriteElementHeader(&simpleTag, mkv.IDTagString, 4)
	ebml.WriteString(&simpleTag, "Test")
	writeUnknown(&simpleTag) // unknown in SimpleTag

	var tag bytes.Buffer
	ebml.WriteElementHeader(&tag, mkv.IDTargets, int64(targets.Len()))
	tag.Write(targets.Bytes())
	ebml.WriteElementHeader(&tag, mkv.IDSimpleTag, int64(simpleTag.Len()))
	tag.Write(simpleTag.Bytes())
	writeUnknown(&tag) // unknown in Tag

	var tags bytes.Buffer
	ebml.WriteElementHeader(&tags, mkv.IDTag, int64(tag.Len()))
	tags.Write(tag.Bytes())
	writeUnknown(&tags) // unknown in Tags

	// Cues with unknowns
	var cueTrackPos bytes.Buffer
	ebml.WriteElementHeader(&cueTrackPos, mkv.IDCueTrack, 1)
	ebml.WriteUint(&cueTrackPos, 1, 1)
	ebml.WriteElementHeader(&cueTrackPos, mkv.IDCueClusterPos, 1)
	ebml.WriteUint(&cueTrackPos, 100, 1)
	writeUnknown(&cueTrackPos)

	var cuePoint bytes.Buffer
	ebml.WriteElementHeader(&cuePoint, mkv.IDCueTime, 1)
	ebml.WriteUint(&cuePoint, 0, 1)
	ebml.WriteElementHeader(&cuePoint, mkv.IDCueTrackPositions, int64(cueTrackPos.Len()))
	cuePoint.Write(cueTrackPos.Bytes())
	writeUnknown(&cuePoint) // unknown in CuePoint

	var cues bytes.Buffer
	ebml.WriteElementHeader(&cues, mkv.IDCuePoint, int64(cuePoint.Len()))
	cues.Write(cuePoint.Bytes())
	writeUnknown(&cues) // unknown in Cues

	// Assemble segment
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTracks, int64(tracks.Len()))
	seg.Write(tracks.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDChapters, int64(chapters.Len()))
	seg.Write(chapters.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDAttachments, int64(attachments.Len()))
	seg.Write(attachments.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTags, int64(tags.Len()))
	seg.Write(tags.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDCues, int64(cues.Len()))
	seg.Write(cues.Bytes())
	// unknown element at segment level
	ebml.WriteElementHeader(&seg, unknownID, 2)
	seg.Write([]byte{0xAB, 0xCD})

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	c, err := Read(context.Background(), r, "unknown.mkv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(c.Tracks))
	}
	if len(c.Chapters) != 1 {
		t.Errorf("chapters = %d, want 1", len(c.Chapters))
	}
	if len(c.Attachments) != 1 {
		t.Errorf("attachments = %d, want 1", len(c.Attachments))
	}
	if len(c.Tags) != 1 {
		t.Errorf("tags = %d, want 1", len(c.Tags))
	}
	if len(c.Cues) != 1 {
		t.Errorf("cues = %d, want 1", len(c.Cues))
	}
}

func TestUnknownSizeElementInSegment(t *testing.T) {
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, 0)
	// Write an element with unknown size (ID=0x42F7, size=-1)
	ebml.WriteElementID(&seg, 0x42F7)
	ebml.WriteDataSize(&seg, -1) // unknown size

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	r := bytes.NewReader(buf.Bytes())
	_, err := Read(context.Background(), r, "unksize.mkv")
	if err == nil {
		t.Fatal("expected error for unknown-size element")
	}
	if !strings.Contains(err.Error(), "unknown-size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// seekFailReader wraps data and fails on non-zero Seek offset after N such calls.
// This specifically targets p.skip() calls which use Seek(size, SeekCurrent) with size>0,
// while allowing Seek(0, SeekCurrent) position queries to succeed.
type seekFailReader struct {
	data           []byte
	pos            int
	skipCount      int
	failAfterSkips int
}

func (r *seekFailReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *seekFailReader) Seek(offset int64, whence int) (int64, error) {
	// Position queries (offset=0, SeekCurrent) always succeed
	if offset == 0 && whence == io.SeekCurrent {
		return int64(r.pos), nil
	}
	// Skip calls (offset>0, SeekCurrent) count toward failure
	if whence == io.SeekCurrent && offset > 0 {
		r.skipCount++
		if r.skipCount > r.failAfterSkips {
			return 0, fmt.Errorf("skip fail #%d", r.skipCount)
		}
	}
	switch whence {
	case io.SeekStart:
		r.pos = int(offset)
	case io.SeekCurrent:
		r.pos += int(offset)
	case io.SeekEnd:
		r.pos = len(r.data) + int(offset)
	}
	if r.pos < 0 {
		r.pos = 0
	}
	if r.pos > len(r.data) {
		r.pos = len(r.data)
	}
	return int64(r.pos), nil
}

func TestSeekFailsInParsers(t *testing.T) {
	// Build a full MKV with unknown elements in every parser section to trigger
	// default:skip branches, then inject seek failures at various points.
	// The skip() calls use Seek, so failing Seek will hit the error returns.
	unknownID := uint32(0x42F7)
	writeUnknown := func(buf *bytes.Buffer) {
		ebml.WriteElementHeader(buf, unknownID, 2)
		buf.Write([]byte{0xAB, 0xCD})
	}

	// Use the same full MKV from TestUnknownElementsInParsers
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 1000000, 3)
	writeUnknown(&info)

	var videoSub bytes.Buffer
	ebml.WriteElementHeader(&videoSub, mkv.IDPixelWidth, 2)
	ebml.WriteUint(&videoSub, 640, 2)
	writeUnknown(&videoSub)

	var audioSub bytes.Buffer
	ebml.WriteElementHeader(&audioSub, mkv.IDSamplingFreq, 8)
	ebml.WriteFloat(&audioSub, 48000.0)
	writeUnknown(&audioSub)

	var compSub bytes.Buffer
	ebml.WriteElementHeader(&compSub, mkv.IDContentCompSettings, 2)
	ebml.WriteBytes(&compSub, []byte{0x01, 0x02})
	writeUnknown(&compSub)

	var encSub bytes.Buffer
	ebml.WriteElementHeader(&encSub, mkv.IDContentCompression, int64(compSub.Len()))
	encSub.Write(compSub.Bytes())
	writeUnknown(&encSub)

	var encsSub bytes.Buffer
	ebml.WriteElementHeader(&encsSub, mkv.IDContentEncoding, int64(encSub.Len()))
	encsSub.Write(encSub.Bytes())
	writeUnknown(&encsSub)

	var trackV bytes.Buffer
	ebml.WriteElementHeader(&trackV, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&trackV, 1, 1)
	ebml.WriteElementHeader(&trackV, mkv.IDTrackType, 1)
	ebml.WriteUint(&trackV, mkv.TrackTypeVideo, 1)
	ebml.WriteElementHeader(&trackV, mkv.IDCodecID, 14)
	ebml.WriteString(&trackV, "V_MPEG4/ISO/AVC")
	ebml.WriteElementHeader(&trackV, mkv.IDVideo, int64(videoSub.Len()))
	trackV.Write(videoSub.Bytes())
	ebml.WriteElementHeader(&trackV, mkv.IDContentEncodings, int64(encsSub.Len()))
	trackV.Write(encsSub.Bytes())
	writeUnknown(&trackV)

	var trackA bytes.Buffer
	ebml.WriteElementHeader(&trackA, mkv.IDTrackNumber, 1)
	ebml.WriteUint(&trackA, 2, 1)
	ebml.WriteElementHeader(&trackA, mkv.IDTrackType, 1)
	ebml.WriteUint(&trackA, mkv.TrackTypeAudio, 1)
	ebml.WriteElementHeader(&trackA, mkv.IDCodecID, 6)
	ebml.WriteString(&trackA, "A_OPUS")
	ebml.WriteElementHeader(&trackA, mkv.IDAudio, int64(audioSub.Len()))
	trackA.Write(audioSub.Bytes())

	var tracks bytes.Buffer
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(trackV.Len()))
	tracks.Write(trackV.Bytes())
	ebml.WriteElementHeader(&tracks, mkv.IDTrackEntry, int64(trackA.Len()))
	tracks.Write(trackA.Bytes())
	writeUnknown(&tracks)

	var chapDisplay bytes.Buffer
	ebml.WriteElementHeader(&chapDisplay, mkv.IDChapString, 3)
	ebml.WriteString(&chapDisplay, "Ch1")
	writeUnknown(&chapDisplay)

	var chapAtom bytes.Buffer
	ebml.WriteElementHeader(&chapAtom, mkv.IDChapterUID, 1)
	ebml.WriteUint(&chapAtom, 1, 1)
	ebml.WriteElementHeader(&chapAtom, mkv.IDChapterTimeStart, 1)
	ebml.WriteUint(&chapAtom, 0, 1)
	ebml.WriteElementHeader(&chapAtom, mkv.IDChapterDisplay, int64(chapDisplay.Len()))
	chapAtom.Write(chapDisplay.Bytes())
	writeUnknown(&chapAtom)

	var edition bytes.Buffer
	ebml.WriteElementHeader(&edition, mkv.IDChapterAtom, int64(chapAtom.Len()))
	edition.Write(chapAtom.Bytes())
	writeUnknown(&edition)

	var chapters bytes.Buffer
	ebml.WriteElementHeader(&chapters, mkv.IDEditionEntry, int64(edition.Len()))
	chapters.Write(edition.Bytes())
	writeUnknown(&chapters)

	var attFile bytes.Buffer
	ebml.WriteElementHeader(&attFile, mkv.IDFileUID, 1)
	ebml.WriteUint(&attFile, 1, 1)
	ebml.WriteElementHeader(&attFile, mkv.IDFileName, 5)
	ebml.WriteString(&attFile, "f.ttf")
	writeUnknown(&attFile)

	var attachments bytes.Buffer
	ebml.WriteElementHeader(&attachments, mkv.IDAttachedFile, int64(attFile.Len()))
	attachments.Write(attFile.Bytes())
	writeUnknown(&attachments)

	var targets bytes.Buffer
	ebml.WriteElementHeader(&targets, mkv.IDTargetType, 5)
	ebml.WriteString(&targets, "MOVIE")
	writeUnknown(&targets)

	var simpleTag bytes.Buffer
	ebml.WriteElementHeader(&simpleTag, mkv.IDTagName, 5)
	ebml.WriteString(&simpleTag, "TITLE")
	writeUnknown(&simpleTag)

	var tag bytes.Buffer
	ebml.WriteElementHeader(&tag, mkv.IDTargets, int64(targets.Len()))
	tag.Write(targets.Bytes())
	ebml.WriteElementHeader(&tag, mkv.IDSimpleTag, int64(simpleTag.Len()))
	tag.Write(simpleTag.Bytes())
	writeUnknown(&tag)

	var tags bytes.Buffer
	ebml.WriteElementHeader(&tags, mkv.IDTag, int64(tag.Len()))
	tags.Write(tag.Bytes())
	writeUnknown(&tags)

	var cueTrackPos bytes.Buffer
	ebml.WriteElementHeader(&cueTrackPos, mkv.IDCueTrack, 1)
	ebml.WriteUint(&cueTrackPos, 1, 1)
	writeUnknown(&cueTrackPos)

	var cuePoint bytes.Buffer
	ebml.WriteElementHeader(&cuePoint, mkv.IDCueTime, 1)
	ebml.WriteUint(&cuePoint, 0, 1)
	ebml.WriteElementHeader(&cuePoint, mkv.IDCueTrackPositions, int64(cueTrackPos.Len()))
	cuePoint.Write(cueTrackPos.Bytes())
	writeUnknown(&cuePoint)

	var cues bytes.Buffer
	ebml.WriteElementHeader(&cues, mkv.IDCuePoint, int64(cuePoint.Len()))
	cues.Write(cuePoint.Bytes())
	writeUnknown(&cues)

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTracks, int64(tracks.Len()))
	seg.Write(tracks.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDChapters, int64(chapters.Len()))
	seg.Write(chapters.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDAttachments, int64(attachments.Len()))
	seg.Write(attachments.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDTags, int64(tags.Len()))
	seg.Write(tags.Bytes())
	ebml.WriteElementHeader(&seg, mkv.IDCues, int64(cues.Len()))
	seg.Write(cues.Bytes())
	writeUnknown(&seg)

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	fullData := buf.Bytes()

	// Fail skip at progressively later points to cover skip error branches
	// in all parser sections.
	for skipLimit := 1; skipLimit < 100; skipLimit++ {
		r := &seekFailReader{data: append([]byte(nil), fullData...), failAfterSkips: skipLimit}
		Read(context.Background(), r, "seekfail.mkv") // errors expected, no panics
	}
}

func TestTruncatedParsers(t *testing.T) {
	// Build a full MKV in memory, then truncate at various points
	w := uint32(1920)
	h := uint32(1080)
	sr := 48000.0
	ch := uint8(2)

	c := &mkv.Container{
		Info: mkv.SegmentInfo{TimecodeScale: 1000000, Title: "Test", MuxingApp: "mkvgo", WritingApp: "mkvgo"},
		Tracks: []mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Language: "eng", IsDefault: true,
				Width: &w, Height: &h, HeaderStripping: []byte{0x00, 0x00, 0x01}},
			{ID: 2, Type: mkv.AudioTrack, Codec: "opus", Language: "fre", SampleRate: &sr, Channels: &ch},
		},
		Chapters: []mkv.Chapter{
			{ID: 1, Title: "Ch1", StartMs: 0, EndMs: 5000},
		},
		Tags: []mkv.Tag{{
			TargetType: "MOVIE", TargetID: 1,
			SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Test"}},
		}},
		Attachments: []mkv.Attachment{
			{ID: 1, Name: "f.ttf", MIMEType: "font/ttf", Data: []byte{0x01, 0x02}},
		},
		Cues: []mkv.CuePoint{
			{TimeMs: 0, Track: 1, ClusterPos: 100},
			{TimeMs: 5000, Track: 1, ClusterPos: 5000},
		},
	}

	var buf bytes.Buffer
	if err := writer.Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	// Also write cues
	if err := writer.WriteCues(&buf, c.Cues, 1000000); err != nil {
		t.Fatal(err)
	}

	// Re-wrap with segment so cues are inside
	var fullBuf bytes.Buffer
	writeEBMLHeader(&fullBuf)
	// Write segment with all inner content
	var seg bytes.Buffer
	if err := writer.WriteSegmentInfo(&seg, &c.Info, 10000); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTracks(&seg, c.Tracks); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteChapters(&seg, c.Chapters); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteAttachments(&seg, c.Attachments); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteTags(&seg, c.Tags); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteCues(&seg, c.Cues, 1000000); err != nil {
		t.Fatal(err)
	}
	writeSegmentStart(&fullBuf, int64(seg.Len()))
	fullBuf.Write(seg.Bytes())
	fullData := fullBuf.Bytes()

	// Truncate at every byte and try to read — should not panic
	for limit := 1; limit < len(fullData); limit++ {
		tr := &truncReader{data: fullData[:limit]}
		Read(context.Background(), tr, "trunc.mkv")
	}
	// Also try with errAtReader (defined in blocks_test.go) that returns errors instead of EOF
	for limit := 1; limit < len(fullData); limit++ {
		tr := &errAtReader{data: fullData[:limit]}
		Read(context.Background(), tr, "trunc.mkv")
	}
}

// TestChapterRecursionDepthLimit guards against unbounded ChapterAtom recursion
// (a crafted deeply-nested chapters file would otherwise stack-overflow).
func TestChapterRecursionDepthLimit(t *testing.T) {
	atom := []byte{} // innermost empty ChapterAtom body
	for i := 0; i < 70; i++ {
		var wrap bytes.Buffer
		ebml.WriteElementHeader(&wrap, mkv.IDChapterAtom, int64(len(atom)))
		wrap.Write(atom)
		atom = wrap.Bytes()
	}
	// Chapters > EditionEntry > (70 nested ChapterAtoms)
	var edition bytes.Buffer
	ebml.WriteElementHeader(&edition, mkv.IDEditionEntry, int64(len(atom)))
	edition.Write(atom)
	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDChapters, int64(edition.Len()))
	seg.Write(edition.Bytes())

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	_, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "deepchap.mkv")
	if err == nil {
		t.Fatal("expected error for deep chapter nesting")
	}
	if !strings.Contains(err.Error(), "chapter nesting") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChargeMeta(t *testing.T) {
	p := &parser{metaBudget: 100}
	if err := p.chargeMeta(60); err != nil {
		t.Fatalf("60/100: %v", err)
	}
	if err := p.chargeMeta(40); err != nil {
		t.Fatalf("exactly-at-budget should pass: %v", err)
	}
	if err := p.chargeMeta(1); err == nil {
		t.Fatal("expected budget-exceeded error")
	}
}

func TestMetadataBudgetRejectsHugeAttachment(t *testing.T) {
	// AttachedFile whose FileData declares 2 GiB (> 1 GiB budget), no body.
	var fd bytes.Buffer
	ebml.WriteElementHeader(&fd, mkv.IDFileData, 2<<30)
	atts := wrapEl(mkv.IDAttachments, wrapEl(mkv.IDAttachedFile, fd.Bytes()))

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(len(atts)))
	buf.Write(atts)

	_, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "huge-att.mkv")
	if err == nil {
		t.Fatal("expected metadata-budget error")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected budget error, got: %v", err)
	}
}
