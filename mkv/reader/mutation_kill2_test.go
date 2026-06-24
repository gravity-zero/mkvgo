package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// nestedChapters returns the outermost ChapterAtom of a chain `levels` deep.
// When parsed from parseStreamChapterAtom(depth=0), the innermost atom is at
// depth = levels-1.
func nestedChapters(levels int) []byte {
	inner := masterElem(mkv.IDChapterAtom, uintElem(mkv.IDChapterUID, 1, 1))
	for i := 2; i <= levels; i++ {
		inner = masterElem(mkv.IDChapterAtom, uintElem(mkv.IDChapterUID, uint64(i), 1), inner)
	}
	return inner
}

// nestedSimpleTags returns the outermost SimpleTag of a chain `levels` deep.
func nestedSimpleTags(levels int) []byte {
	inner := masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "L0"))
	for i := 2; i <= levels; i++ {
		inner = masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "L0"), inner)
	}
	return inner
}

// streamFixture wraps a pre-built segment body (Info + Tracks + extras, WITHOUT
// a cluster) into a complete streamable MKV. realCluster() is appended inside
// the segment so that ReadStream stops at the first cluster and returns.
func streamFixture(segBody []byte) []byte {
	body := append(segBody, realCluster()...)
	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementHeader(&full, mkv.IDSegment, int64(len(body)))
	full.Write(body)
	return full.Bytes()
}

// readMKV calls Read on data and returns (c, err).
func readMKV(data []byte) (*mkv.Container, error) {
	return Read(context.Background(), bytes.NewReader(data), "test.mkv")
}

// ── stream.go mutation kills ──────────────────────────────────────────────────

// TestReadStream2InfoTextFields kills stream.go:271/277/283
// (CONDITIONALS_NEGATION on if err != nil after readString for MuxingApp/WritingApp/Title).
// With the mutation, success returns nil before assigning the field.
func TestReadStream2InfoTextFields(t *testing.T) {
	var info bytes.Buffer
	info.Write(uintElem(mkv.IDTimecodeScale, 1_000_000, 3))
	info.Write(strElem(mkv.IDMuxingApp, "mkvgo-test"))
	info.Write(strElem(mkv.IDWritingApp, "wapp"))
	info.Write(strElem(mkv.IDTitle, "My Title"))

	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
	)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, info.Bytes()))
	seg.Write(masterElem(mkv.IDTracks, te))

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if c.Info.MuxingApp != "mkvgo-test" {
		t.Errorf("MuxingApp = %q, want %q", c.Info.MuxingApp, "mkvgo-test")
	}
	if c.Info.WritingApp != "wapp" {
		t.Errorf("WritingApp = %q, want %q", c.Info.WritingApp, "wapp")
	}
	if c.Info.Title != "My Title" {
		t.Errorf("Title = %q, want %q", c.Info.Title, "My Title")
	}
}

// TestReadStream2TrackFields kills stream.go:320/349/355/369
// (CONDITIONALS_NEGATION on TrackUID/CodecPrivate/Language/Name).
func TestReadStream2TrackFields(t *testing.T) {
	codecPriv := []byte{0x01, 0x02, 0x03, 0x04}
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		uintElem(mkv.IDTrackUID, 99, 1),
		bytesElem(mkv.IDCodecPrivate, codecPriv),
		strElem(mkv.IDLanguage, "fra"),
		strElem(mkv.IDName, "Video Main"),
	)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks, te))

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	tr := c.Tracks[0]
	if tr.UID != 99 {
		t.Errorf("UID = %d, want 99", tr.UID)
	}
	if !bytes.Equal(tr.CodecPrivate, codecPriv) {
		t.Errorf("CodecPrivate = %v, want %v", tr.CodecPrivate, codecPriv)
	}
	if tr.Language != "fra" {
		t.Errorf("Language = %q, want %q", tr.Language, "fra")
	}
	if tr.Name != "Video Main" {
		t.Errorf("Name = %q, want %q", tr.Name, "Video Main")
	}
}

// TestReadStream2DefaultDurationZero kills stream.go:392
// (CONDITIONALS_BOUNDARY: if v > 0 → if v >= 0 always true for uint64).
// v=0 must NOT set FrameRate (would be +Inf with mutation).
func TestReadStream2DefaultDurationZero(t *testing.T) {
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		uintElem(mkv.IDDefaultDuration, 0, 1),
	)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks, te))

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	if c.Tracks[0].FrameRate != nil {
		t.Errorf("FrameRate with DefaultDuration=0 must be nil, got %v", *c.Tracks[0].FrameRate)
	}
}

// TestReadStream2VideoFields kills stream.go:462/469/476
// (CONDITIONALS_NEGATION on Width/Height/FlagInterlaced).
func TestReadStream2VideoFields(t *testing.T) {
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDPixelWidth, 1920, 2),
		uintElem(mkv.IDPixelHeight, 1080, 2),
		uintElem(mkv.IDFlagInterlaced, 1, 1),
	)
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		video,
	)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks, te))

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	tr := c.Tracks[0]
	if tr.Width == nil || *tr.Width != 1920 {
		t.Errorf("Width = %v, want 1920", tr.Width)
	}
	if tr.Height == nil || *tr.Height != 1080 {
		t.Errorf("Height = %v, want 1080", tr.Height)
	}
	if tr.FieldOrder != "interlaced" {
		t.Errorf("FieldOrder = %q, want %q", tr.FieldOrder, "interlaced")
	}
}

// TestReadStream2ColourBitsPerChannel kills stream.go:538
// (CONDITIONALS_NEGATION on IDColourBitsPerChannel).
func TestReadStream2ColourBitsPerChannel(t *testing.T) {
	colour := masterElem(mkv.IDColour,
		uintElem(mkv.IDColourBitsPerChannel, 10, 1),
	)
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDPixelWidth, 1280, 2),
		uintElem(mkv.IDPixelHeight, 720, 2),
		colour,
	)
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		video,
	)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks, te))

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	tr := c.Tracks[0]
	if tr.VideoBitDepth == nil || *tr.VideoBitDepth != 10 {
		t.Errorf("VideoBitDepth = %v, want 10", tr.VideoBitDepth)
	}
}

// TestReadStream2AudioFields kills stream.go:555/567/574
// (CONDITIONALS_NEGATION on SamplingFreq/OutputSamplingFreq/Channels).
func TestReadStream2AudioFields(t *testing.T) {
	audio := masterElem(mkv.IDAudio,
		floatElem(mkv.IDSamplingFreq, 48000.0),
		floatElem(mkv.IDOutputSamplingFreq, 96000.0),
		uintElem(mkv.IDChannels, 6, 1),
		uintElem(mkv.IDBitDepth, 24, 1),
	)
	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		audio,
	)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks, te))

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	tr := c.Tracks[0]
	if tr.SampleRate == nil || *tr.SampleRate != 48000.0 {
		t.Errorf("SampleRate = %v, want 48000", tr.SampleRate)
	}
	if tr.OutputSampleRate == nil || *tr.OutputSampleRate != 96000.0 {
		t.Errorf("OutputSampleRate = %v, want 96000", tr.OutputSampleRate)
	}
	if tr.Channels == nil || *tr.Channels != 6 {
		t.Errorf("Channels = %v, want 6", tr.Channels)
	}
}

// TestReadStream2ChapterDepth65OK kills stream.go:610
// (CONDITIONALS_BOUNDARY: depth > maxChapterDepth → depth >= maxChapterDepth).
// 65 levels (innermost depth=64) must succeed with original.
func TestReadStream2ChapterDepth65OK(t *testing.T) {
	chapBody := nestedChapters(maxChapterDepth + 1) // 65 levels, deepest at depth=64
	chapters := masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, chapBody))

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(chapters)

	_, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("65-level nesting must succeed, got: %v", err)
	}
}

// TestReadStream2ChapterDepth66Error kills stream.go:637
// (ARITHMETIC_BASE: depth+1 → depth or depth+0, never incrementing).
// 66 levels must return an error because the innermost is at depth=65 > maxChapterDepth.
func TestReadStream2ChapterDepth66Error(t *testing.T) {
	chapBody := nestedChapters(maxChapterDepth + 2) // 66 levels, deepest at depth=65
	chapters := masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, chapBody))

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(chapters)

	_, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err == nil {
		t.Fatal("66-level nesting must return error (maxChapterDepth exceeded)")
	}
}

// TestReadStream2ChapterFields kills stream.go:618/624/627/630/633
// (CONDITIONALS_NEGATION on ChapterUID/TimeStart/TimeEnd) and
// the ARITHMETIC_BASE mutations on int64(v/1_000_000) for StartMs/EndMs.
func TestReadStream2ChapterFields(t *testing.T) {
	// TimeStart = 5_000_000_000 ns → 5000 ms; TimeEnd = 10_000_000_000 ns → 10000 ms
	chap := masterElem(mkv.IDChapterAtom,
		uintElem(mkv.IDChapterUID, 42, 1),
		uintElem(mkv.IDChapterTimeStart, 5_000_000_000, 5),
		uintElem(mkv.IDChapterTimeEnd, 10_000_000_000, 5),
	)
	chapters := masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, chap))

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(chapters)

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	ch := c.Chapters[0]
	if ch.ID != 42 {
		t.Errorf("chapter ID = %d, want 42", ch.ID)
	}
	if ch.StartMs != 5000 {
		t.Errorf("StartMs = %d, want 5000 (5_000_000_000 / 1_000_000)", ch.StartMs)
	}
	if ch.EndMs != 10000 {
		t.Errorf("EndMs = %d, want 10000 (10_000_000_000 / 1_000_000)", ch.EndMs)
	}
}

// TestReadStream2ChapterTitle kills stream.go:652/654
// (CONDITIONALS_NEGATION on IDChapString check and its err != nil guard).
func TestReadStream2ChapterTitle(t *testing.T) {
	display := masterElem(mkv.IDChapterDisplay, strElem(mkv.IDChapString, "Chapter One"))
	chap := masterElem(mkv.IDChapterAtom,
		uintElem(mkv.IDChapterUID, 1, 1),
		display,
	)
	chapters := masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, chap))

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(chapters)

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	if c.Chapters[0].Title != "Chapter One" {
		t.Errorf("Title = %q, want %q", c.Chapters[0].Title, "Chapter One")
	}
}

// TestReadStream2ChapterSubAtom kills stream.go:637 (nested IDChapterAtom inside
// parseStreamChapterAtom) — distinct from the depth arithmetic test.
// The outer chapter must have a SubChapters slice of length 1.
func TestReadStream2ChapterSubAtom(t *testing.T) {
	inner := masterElem(mkv.IDChapterAtom, uintElem(mkv.IDChapterUID, 2, 1))
	outer := masterElem(mkv.IDChapterAtom, uintElem(mkv.IDChapterUID, 1, 1), inner)
	chapters := masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, outer))

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(chapters)

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	if len(c.Chapters[0].SubChapters) != 1 {
		t.Errorf("SubChapters = %d, want 1", len(c.Chapters[0].SubChapters))
	}
}

// TestReadStream2AttachmentFields kills stream.go:684 (IDAttachedFile NEGATION).
func TestReadStream2AttachmentFields(t *testing.T) {
	att := masterElem(mkv.IDAttachedFile,
		uintElem(mkv.IDFileUID, 7, 1),
		strElem(mkv.IDFileName, "test.ttf"),
		strElem(mkv.IDFileMimeType, "font/ttf"),
		bytesElem(mkv.IDFileData, []byte{0x01, 0x02, 0x03}),
	)
	attachments := masterElem(mkv.IDAttachments, att)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(attachments)

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(c.Attachments))
	}
	a := c.Attachments[0]
	if a.ID != 7 {
		t.Errorf("ID = %d, want 7", a.ID)
	}
	if a.Name != "test.ttf" {
		t.Errorf("Name = %q, want %q", a.Name, "test.ttf")
	}
	if a.MIMEType != "font/ttf" {
		t.Errorf("MIMEType = %q, want %q", a.MIMEType, "font/ttf")
	}
}

// TestReadStream2SimpleTagDepth65OK kills stream.go:772
// (CONDITIONALS_BOUNDARY: depth > maxTagDepth → depth >= maxTagDepth).
// nestedSimpleTags(65) returns an outermost IDSimpleTag; parsed at depth=0,
// the innermost is at depth=64. Original (depth>64) succeeds; mutation (>=64) errors.
func TestReadStream2SimpleTagDepth65OK(t *testing.T) {
	st := nestedSimpleTags(maxTagDepth + 1) // 65 levels; innermost at depth=64
	tag := masterElem(mkv.IDTag, st)        // no extra wrapper — outermost is depth=0
	tags := masterElem(mkv.IDTags, tag)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(tags)

	_, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("65-level SimpleTag nesting must succeed, got: %v", err)
	}
}

// TestReadStream2SimpleTagDepth66Error kills stream.go:803
// (ARITHMETIC_BASE: depth+1 → depth, never incrementing, depth limit never fires).
func TestReadStream2SimpleTagDepth66Error(t *testing.T) {
	st := nestedSimpleTags(maxTagDepth + 2) // 66 levels; innermost at depth=65 > 64
	tag := masterElem(mkv.IDTag, st)        // no extra wrapper — outermost is depth=0
	tags := masterElem(mkv.IDTags, tag)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(tags)

	_, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err == nil {
		t.Fatal("66-level SimpleTag nesting must return error (maxTagDepth exceeded)")
	}
}

// TestReadStream2SimpleTagNested kills stream.go:798
// (CONDITIONALS_NEGATION on IDSimpleTag case in parseStreamSimpleTag).
func TestReadStream2SimpleTagNested(t *testing.T) {
	// Outer SimpleTag with a sub-SimpleTag inside.
	inner := masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "INNER"))
	outer := masterElem(mkv.IDSimpleTag,
		strElem(mkv.IDTagName, "OUTER"),
		inner,
	)
	tag := masterElem(mkv.IDTag, outer)
	tags := masterElem(mkv.IDTags, tag)

	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	seg.Write(masterElem(mkv.IDTracks,
		masterElem(mkv.IDTrackEntry,
			uintElem(mkv.IDTrackNumber, 1, 1),
			uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		),
	))
	seg.Write(tags)

	c, _, err := ReadStream(context.Background(), bytes.NewReader(streamFixture(seg.Bytes())))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tags) == 0 || len(c.Tags[0].SimpleTags) == 0 {
		t.Fatal("no tags/simple-tags")
	}
	if len(c.Tags[0].SimpleTags[0].SubTags) != 1 {
		t.Errorf("SubTags = %d, want 1", len(c.Tags[0].SimpleTags[0].SubTags))
	}
}

// TestReadStream2UnknownSizeCluster kills stream.go:232
// (INVERT_NEGATIVES/ARITHMETIC_BASE on clusterEnd: -1 in BlockReader struct literal).
// With mutation (clusterEnd=1 or 0), the cluster ends immediately and no blocks are read.
func TestReadStream2UnknownSizeCluster(t *testing.T) {
	var info bytes.Buffer
	ebml.WriteElementHeader(&info, mkv.IDTimecodeScale, 3)
	ebml.WriteUint(&info, 1_000_000, 3)

	te := masterElem(mkv.IDTrackEntry,
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
	)

	// Build an unknown-size cluster with one block.
	payload := []byte{0x81, 0x00, 0x00, 0x80, 0xAA} // track=1, relTC=0, keyframe, data=0xAA

	var seg bytes.Buffer
	ebml.WriteElementHeader(&seg, mkv.IDInfo, int64(info.Len()))
	seg.Write(info.Bytes())
	seg.Write(masterElem(mkv.IDTracks, te))

	// Unknown-size cluster.
	ebml.WriteElementID(&seg, mkv.IDCluster)
	ebml.WriteDataSize(&seg, -1)
	ebml.WriteElementHeader(&seg, mkv.IDTimestamp, 1)
	ebml.WriteUint(&seg, 0, 1)
	ebml.WriteElementHeader(&seg, mkv.IDSimpleBlock, int64(len(payload)))
	seg.Write(payload)

	var full bytes.Buffer
	ebml.WriteElementHeader(&full, ebml.IDEBMLHeader, 0)
	ebml.WriteElementID(&full, mkv.IDSegment)
	ebml.WriteDataSize(&full, -1) // unknown-size segment
	full.Write(seg.Bytes())

	c, br, err := ReadStream(context.Background(), bytes.NewReader(full.Bytes()))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}

	// Must be able to read at least one block from the unknown-size cluster.
	b, err := br.Next()
	if errors.Is(err, io.EOF) {
		t.Fatal("expected at least one block from unknown-size cluster, got EOF (mutation: clusterEnd initialised to non-negative)")
	}
	if err != nil {
		t.Fatalf("br.Next: %v", err)
	}
	if b.TrackNumber != 1 {
		t.Errorf("block track = %d, want 1", b.TrackNumber)
	}
}

// ── reader.go mutation kills ──────────────────────────────────────────────────

// TestOpen2ValidFile kills reader.go:19
// (CONDITIONALS_NEGATION on if err != nil after os.Open).
// With mutation, a successful Open returns an error before calling Read.
func TestOpen2ValidFile(t *testing.T) {
	data := buildMinimalMKV().Bytes()
	path := t.TempDir() + "/valid.mkv"
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c == nil {
		t.Fatal("Open returned nil container")
	}
}

// TestRead2DefaultDurationZero kills reader.go:510
// (CONDITIONALS_BOUNDARY: if v > 0 → if v >= 0 for DefaultDuration).
func TestRead2DefaultDurationZero(t *testing.T) {
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		uintElem(mkv.IDDefaultDuration, 0, 1),
	)
	c, err := Read(context.Background(), bytes.NewReader(buildMKV(te)), "test.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	if c.Tracks[0].FrameRate != nil {
		t.Errorf("FrameRate with DefaultDuration=0 must be nil, got %v", *c.Tracks[0].FrameRate)
	}
}

// TestRead2VideoFields kills reader.go:598/617/621
// (CONDITIONALS_NEGATION on FlagInterlaced/DisplayWidth/DisplayHeight).
func TestRead2VideoFields(t *testing.T) {
	video := masterElem(mkv.IDVideo,
		uintElem(mkv.IDFlagInterlaced, 1, 1),
		uintElem(mkv.IDDisplayWidth, 640, 2),
		uintElem(mkv.IDDisplayHeight, 480, 2),
	)
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		video,
	)
	c, err := Read(context.Background(), bytes.NewReader(buildMKV(te)), "test.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tracks) == 0 {
		t.Fatal("no tracks")
	}
	tr := c.Tracks[0]
	if tr.FieldOrder != "interlaced" {
		t.Errorf("FieldOrder = %q, want %q", tr.FieldOrder, "interlaced")
	}
	if tr.DisplayWidth == nil || *tr.DisplayWidth != 640 {
		t.Errorf("DisplayWidth = %v, want 640", tr.DisplayWidth)
	}
	if tr.DisplayHeight == nil || *tr.DisplayHeight != 480 {
		t.Errorf("DisplayHeight = %v, want 480", tr.DisplayHeight)
	}
}

// TestRead2ChapterDepth65OK kills reader.go:848
// (CONDITIONALS_BOUNDARY: depth > maxChapterDepth → depth >= maxChapterDepth).
func TestRead2ChapterDepth65OK(t *testing.T) {
	chapBody := nestedChapters(maxChapterDepth + 1)
	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, chapBody)))

	_, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("65-level nesting must succeed, got: %v", err)
	}
}

// TestRead2ChapterTimes kills reader.go:875/881
// (ARITHMETIC_BASE on int64(v / 1000000) for StartMs and EndMs).
func TestRead2ChapterTimes(t *testing.T) {
	chap := masterElem(mkv.IDChapterAtom,
		uintElem(mkv.IDChapterUID, 1, 1),
		uintElem(mkv.IDChapterTimeStart, 3_000_000_000, 4),
		uintElem(mkv.IDChapterTimeEnd, 6_000_000_000, 5),
	)
	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, chap)))

	c, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	ch := c.Chapters[0]
	if ch.StartMs != 3000 {
		t.Errorf("StartMs = %d, want 3000 (3_000_000_000 / 1_000_000)", ch.StartMs)
	}
	if ch.EndMs != 6000 {
		t.Errorf("EndMs = %d, want 6000 (6_000_000_000 / 1_000_000)", ch.EndMs)
	}
}

// TestRead2ChapterSubAtomAndDisplay kills reader.go:899/926.
// Line 899: IDChapterAtom nested (sub-chapters must be populated).
// Line 926: IDChapterDisplay skip → to kill, a skip-triggering element must precede
// IDChapString so that the mutation (return nil after skip) prevents title parsing.
func TestRead2ChapterSubAtomAndDisplay(t *testing.T) {
	// ChapterDisplay with IDChapLanguage (skipped) BEFORE IDChapString.
	// With mutation at 926, skip succeeds → return nil → IDChapString never reached.
	display := masterElem(mkv.IDChapterDisplay,
		strElem(mkv.IDChapLanguage, "eng"),
		strElem(mkv.IDChapString, "Chapter One"),
	)
	inner := masterElem(mkv.IDChapterAtom, uintElem(mkv.IDChapterUID, 2, 1))
	outer := masterElem(mkv.IDChapterAtom,
		uintElem(mkv.IDChapterUID, 1, 1),
		display,
		inner,
	)

	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(masterElem(mkv.IDChapters, masterElem(mkv.IDEditionEntry, outer)))

	c, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	ch := c.Chapters[0]
	if ch.Title != "Chapter One" {
		t.Errorf("Title = %q, want %q", ch.Title, "Chapter One")
	}
	if len(ch.SubChapters) != 1 {
		t.Errorf("SubChapters = %d, want 1", len(ch.SubChapters))
	}
}

// TestRead2AttachmentAllFields kills reader.go:953/977/989/998/1004
// (IDAttachedFile and its sub-field CONDITIONALS_NEGATION).
func TestRead2AttachmentAllFields(t *testing.T) {
	att := masterElem(mkv.IDAttachedFile,
		uintElem(mkv.IDFileUID, 7, 1),
		strElem(mkv.IDFileName, "test.ttf"),
		strElem(mkv.IDFileMimeType, "font/ttf"),
		bytesElem(mkv.IDFileData, []byte{0x01, 0x02, 0x03}),
	)

	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(masterElem(mkv.IDAttachments, att))

	c, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(c.Attachments))
	}
	a := c.Attachments[0]
	if a.ID != 7 {
		t.Errorf("ID = %d, want 7", a.ID)
	}
	if a.Name != "test.ttf" {
		t.Errorf("Name = %q, want %q", a.Name, "test.ttf")
	}
	if a.MIMEType != "font/ttf" {
		t.Errorf("MIMEType = %q, want %q", a.MIMEType, "font/ttf")
	}
	if !bytes.Equal(a.Data, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("Data = %v, want [1 2 3]", a.Data)
	}
}

// TestRead2TagAllFields kills reader.go:1031/1064/1098/1111.
// Kills 1031 (parseTags else-skip) by putting IDVoid before IDTag.
// Kills 1064 (parseTag default-skip) by putting IDVoid before IDSimpleTag.
// Kills 1098 (parseTargets default-skip) by putting IDTargetTypeValue before IDTargetType.
// Kills 1111 (parseSimpleTagDepth depth BOUNDARY) via the 65-level test below.
func TestRead2TagAllFields(t *testing.T) {
	// Targets: IDTargetTypeValue (numeric, default/skip) BEFORE IDTargetType + IDTagTrackUID.
	// Mutation at 1098: after skipping IDTargetTypeValue (success), returns nil → IDTargetType unset.
	targets := masterElem(mkv.IDTargets,
		uintElem(mkv.IDTargetTypeValue, 50, 1), // unknown in parseTargets → skip
		strElem(mkv.IDTargetType, "ALBUM"),
		uintElem(mkv.IDTagTrackUID, 42, 1),
	)
	// Tag: IDVoid (default/skip) BEFORE IDTargets and IDSimpleTag.
	// Mutation at 1064: after skipping IDVoid (success), returns tag,nil → IDTargets+IDSimpleTag unset.
	simpleTag := masterElem(mkv.IDSimpleTag, strElem(mkv.IDTagName, "TITLE"), strElem(mkv.IDTagString, "Test Value"))
	tag := masterElem(mkv.IDTag,
		masterElem(mkv.IDVoid), // triggers 1064 kill
		targets,
		simpleTag,
	)
	// Tags: IDVoid BEFORE IDTag.
	// Mutation at 1031: after skipping IDVoid (success), returns nil → IDTag unset.
	tagsElem := masterElem(mkv.IDTags,
		masterElem(mkv.IDVoid), // triggers 1031 kill
		tag,
	)

	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(tagsElem)

	c, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Tags) != 1 {
		t.Fatalf("tags = %d, want 1", len(c.Tags))
	}
	tg := c.Tags[0]
	if tg.TargetType != "ALBUM" {
		t.Errorf("TargetType = %q, want %q", tg.TargetType, "ALBUM")
	}
	if tg.TargetID != 42 {
		t.Errorf("TargetID = %d, want 42", tg.TargetID)
	}
	if len(tg.SimpleTags) != 1 {
		t.Fatalf("SimpleTags = %d, want 1", len(tg.SimpleTags))
	}
	if tg.SimpleTags[0].Name != "TITLE" {
		t.Errorf("SimpleTag.Name = %q, want %q", tg.SimpleTags[0].Name, "TITLE")
	}
	if tg.SimpleTags[0].Value != "Test Value" {
		t.Errorf("SimpleTag.Value = %q, want %q", tg.SimpleTags[0].Value, "Test Value")
	}
}

// TestRead2SimpleTagDepth65OK kills reader.go:1111
// (CONDITIONALS_BOUNDARY: depth > maxTagDepth → depth >= maxTagDepth).
func TestRead2SimpleTagDepth65OK(t *testing.T) {
	st := nestedSimpleTags(maxTagDepth + 1) // 65 levels; no extra wrapper
	tagsElem := masterElem(mkv.IDTags, masterElem(mkv.IDTag, st))

	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(tagsElem)

	_, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("65-level SimpleTag nesting must succeed, got: %v", err)
	}
}

// TestRead2CueFields kills reader.go:1259/1295 and related cue mutations.
// Kills 1259 (IDCuePoint NEGATION in parseCues): CuePoint must appear in c.Cues.
// Kills 1295 (IDCueTrackPositions NEGATION in parseCuePoint): to expose the mutation,
// IDCueTrackPositions comes BEFORE IDCueTime so that the early-return from mutation
// prevents IDCueTime from being parsed.
func TestRead2CueFields(t *testing.T) {
	// CueTrackPositions BEFORE CueTime: if mutation at 1295 fires (return cp,nil after
	// successful parseCueTrackPositions), IDCueTime is never parsed → cp.TimeMs = 0.
	cueTrackPos := masterElem(mkv.IDCueTrackPositions,
		uintElem(mkv.IDCueTrack, 2, 1),
		uintElem(mkv.IDCueClusterPos, 1024, 2),
	)
	cuePoint := masterElem(mkv.IDCuePoint,
		cueTrackPos,                     // processed first → 1295 mutation returns early
		uintElem(mkv.IDCueTime, 500, 2), // only processed if mutation is NOT active
	)
	// IDVoid before IDCuePoint in Cues: kills parseCues skip mutation (line ~1266).
	cues := masterElem(mkv.IDCues,
		masterElem(mkv.IDVoid), // skip → mutation at cues-skip returns nil → IDCuePoint never parsed
		cuePoint,
	)

	var segBody bytes.Buffer
	segBody.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 3)))
	segBody.Write(cues)

	c, err := readMKV(wrapSegment(segBody.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(c.Cues) != 1 {
		t.Fatalf("cues = %d, want 1", len(c.Cues))
	}
	cp := c.Cues[0]
	if cp.Track != 2 {
		t.Errorf("Track = %d, want 2", cp.Track)
	}
	if cp.ClusterPos != 1024 {
		t.Errorf("ClusterPos = %d, want 1024", cp.ClusterPos)
	}
	if cp.TimeMs != 500 {
		t.Errorf("TimeMs = %d, want 500", cp.TimeMs)
	}
}
