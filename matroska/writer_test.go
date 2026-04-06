package matroska

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

func TestWriteEBMLHeader(t *testing.T) {
	var buf bytes.Buffer
	assertNoErr(t, writer.WriteEBMLHeader(&buf))

	r := bytes.NewReader(buf.Bytes())
	hdr, _, err := ebml.ReadElementHeader(r)
	assertNoErr(t, err)
	assertEqual(t, hdr.ID, uint32(ebml.IDEBMLHeader), "header ID")

	found := map[uint32]uint64{}
	var docType string
	for {
		eh, _, err := ebml.ReadElementHeader(r)
		if err != nil {
			break
		}
		if eh.ID == ebml.IDDocType {
			s, _ := ebml.ReadString(r, eh.Size)
			docType = s
		} else {
			v, _ := ebml.ReadUint(r, eh.Size)
			found[eh.ID] = v
		}
	}

	assertEqual(t, docType, "matroska", "DocType")
	assertEqual(t, found[ebml.IDEBMLVersion], uint64(1), "EBMLVersion")
	assertEqual(t, found[ebml.IDEBMLReadVersion], uint64(1), "EBMLReadVersion")
	assertEqual(t, found[ebml.IDEBMLMaxIDLength], uint64(4), "EBMLMaxIDLength")
	assertEqual(t, found[ebml.IDEBMLMaxSizeLength], uint64(8), "EBMLMaxSizeLength")
	assertEqual(t, found[ebml.IDDocTypeVersion], uint64(4), "DocTypeVersion")
	assertEqual(t, found[ebml.IDDocTypeReadVersion], uint64(2), "DocTypeReadVersion")
}

func TestWriteSegmentInfo(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	info := SegmentInfo{
		Title: "Test Movie", MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test",
		Duration: 5000.0, TimecodeScale: 1000000, DateUTC: &now,
	}

	var seg bytes.Buffer
	assertNoErr(t, writer.WriteSegmentInfo(&seg, &info, 0))

	var buf bytes.Buffer
	assertNoErr(t, writer.WriteEBMLHeader(&buf))
	assertNoErr(t, writer.WriteMasterElement(&buf, mkv.IDSegment, seg.Bytes()))
	got, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "test.mkv")
	assertNoErr(t, err)

	assertEqual(t, got.Info.Title, "Test Movie", "Title")
	assertEqual(t, got.Info.MuxingApp, "mkvgo-test", "MuxingApp")
	assertEqual(t, got.Info.WritingApp, "mkvgo-test", "WritingApp")
	assertEqual(t, got.Info.Duration, 5000.0, "Duration")
	assertEqual(t, got.Info.TimecodeScale, int64(1000000), "TimecodeScale")
	if got.Info.DateUTC == nil || !got.Info.DateUTC.Equal(now) {
		t.Errorf("DateUTC = %v, want %v", got.Info.DateUTC, now)
	}
}

func TestWriteTracks(t *testing.T) {
	w1920, h1080 := uint32(1920), uint32(1080)
	sr, ch, bd := 48000.0, uint8(6), uint8(24)

	tracks := []Track{
		{ID: 1, Type: VideoTrack, Codec: "hevc", Language: "und", Name: "Main Video", IsDefault: true, Width: &w1920, Height: &h1080},
		{ID: 2, Type: AudioTrack, Codec: "aac", Language: "fre", Name: "French Audio", IsDefault: true, SampleRate: &sr, Channels: &ch, BitDepth: &bd},
		{ID: 3, Type: SubtitleTrack, Codec: "srt", Language: "eng", Name: "English Subs", IsDefault: false, IsForced: true},
	}

	got := writeAndParse(t, func(w io.Writer) error { return writer.WriteTracks(w, tracks) })

	if len(got.Tracks) != 3 {
		t.Fatalf("got %d tracks, want 3", len(got.Tracks))
	}

	v := got.Tracks[0]
	assertEqual(t, v.Type, VideoTrack, "track[0].Type")
	assertEqual(t, v.Codec, "hevc", "track[0].Codec")
	assertEqual(t, v.Name, "Main Video", "track[0].Name")
	assertEqual(t, *v.Width, uint32(1920), "track[0].Width")
	assertEqual(t, *v.Height, uint32(1080), "track[0].Height")

	a := got.Tracks[1]
	assertEqual(t, a.Type, AudioTrack, "track[1].Type")
	assertEqual(t, a.Codec, "aac", "track[1].Codec")
	assertEqual(t, a.Language, "fre", "track[1].Language")
	assertEqual(t, *a.SampleRate, 48000.0, "track[1].SampleRate")
	assertEqual(t, *a.Channels, uint8(6), "track[1].Channels")
	assertEqual(t, *a.BitDepth, uint8(24), "track[1].BitDepth")

	s := got.Tracks[2]
	assertEqual(t, s.Type, SubtitleTrack, "track[2].Type")
	assertEqual(t, s.IsDefault, false, "track[2].IsDefault")
	assertEqual(t, s.IsForced, true, "track[2].IsForced")
}

func TestWriteChapters(t *testing.T) {
	chapters := []Chapter{
		{ID: 1, Title: "Intro", StartMs: 0, EndMs: 5000},
		{ID: 2, Title: "Main", StartMs: 5000, EndMs: 120000},
		{ID: 3, Title: "Credits", StartMs: 120000},
	}
	got := writeAndParse(t, func(w io.Writer) error { return writer.WriteChapters(w, chapters) })
	assertDeepEqual(t, got.Chapters, chapters, "chapters")
}

func TestWriteTags(t *testing.T) {
	tags := []Tag{
		{TargetType: "MOVIE", SimpleTags: []SimpleTag{
			{Name: "TITLE", Value: "Test Movie", Language: "eng"},
			{Name: "DIRECTOR", Value: "John Doe"},
		}},
		{TargetID: 2, SimpleTags: []SimpleTag{
			{Name: "LANGUAGE", Value: "French", Language: "fre"},
		}},
	}
	got := writeAndParse(t, func(w io.Writer) error { return writer.WriteTags(w, tags) })
	assertDeepEqual(t, got.Tags, tags, "tags")
}

func TestWriteAttachments(t *testing.T) {
	attachments := []Attachment{
		{ID: 1, Name: "cover.jpg", MIMEType: "image/jpeg", Size: 4, Data: []byte{0xFF, 0xD8, 0xFF, 0xE0}},
		{ID: 2, Name: "font.ttf", MIMEType: "font/ttf", Size: 4, Data: []byte{0x00, 0x01, 0x00, 0x00}},
	}
	got := writeAndParse(t, func(w io.Writer) error { return writer.WriteAttachments(w, attachments) })
	assertDeepEqual(t, got.Attachments, attachments, "attachments")
}

func TestWriteRoundTrip(t *testing.T) {
	w1920, h1080 := uint32(1920), uint32(1080)
	sr, ch := 48000.0, uint8(2)
	now := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)

	original := &Container{
		Info: SegmentInfo{
			Title: "Round Trip Test", MuxingApp: "mkvgo", WritingApp: "mkvgo",
			Duration: 60000.0, TimecodeScale: 1000000, DateUTC: &now,
		},
		Tracks: []Track{
			{ID: 1, Type: VideoTrack, Codec: "hevc", Language: "und", IsDefault: true, Width: &w1920, Height: &h1080},
			{ID: 2, Type: AudioTrack, Codec: "opus", Language: "eng", IsDefault: true, SampleRate: &sr, Channels: &ch},
			{ID: 3, Type: SubtitleTrack, Codec: "srt", Language: "fre", IsDefault: false, IsForced: true, Name: "Forced FR"},
		},
		Chapters:    []Chapter{{ID: 1, Title: "Opening", StartMs: 0, EndMs: 5000}, {ID: 2, Title: "Main", StartMs: 5000, EndMs: 55000}, {ID: 3, Title: "End", StartMs: 55000}},
		Tags:        []Tag{{TargetType: "MOVIE", SimpleTags: []SimpleTag{{Name: "TITLE", Value: "Round Trip", Language: "eng"}}}},
		Attachments: []Attachment{{ID: 1, Name: "poster.png", MIMEType: "image/png", Size: 4, Data: []byte{0x89, 0x50, 0x4E, 0x47}}},
		DurationMs:  60000,
	}

	var buf bytes.Buffer
	assertNoErr(t, Write(&buf, original))
	got, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "test.mkv")
	assertNoErr(t, err)

	assertEqual(t, got.Info.Title, "Round Trip Test", "Title")
	assertEqual(t, got.Info.Duration, 60000.0, "Duration")
	assertEqual(t, got.DurationMs, int64(60000), "DurationMs")
	assertEqual(t, len(got.Tracks), 3, "tracks count")
	assertDeepEqual(t, got.Chapters, original.Chapters, "chapters")
	assertDeepEqual(t, got.Tags, original.Tags, "tags")
	assertDeepEqual(t, got.Attachments, original.Attachments, "attachments")

	for i, tr := range got.Tracks {
		assertEqual(t, tr.Type, original.Tracks[i].Type, "track type")
		assertEqual(t, tr.Codec, original.Tracks[i].Codec, "track codec")
	}
}

func TestWriteFromRealMKV(t *testing.T) {
	original, err := Open(context.Background(), fixturePath)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}

	var buf bytes.Buffer
	assertNoErr(t, Write(&buf, original))
	got, err := Read(context.Background(), bytes.NewReader(buf.Bytes()), "rewritten.mkv")
	assertNoErr(t, err)

	assertEqual(t, got.Info.Title, original.Info.Title, "Title")
	assertEqual(t, got.Info.MuxingApp, original.Info.MuxingApp, "MuxingApp")
	assertEqual(t, got.Info.WritingApp, original.Info.WritingApp, "WritingApp")
	assertEqual(t, len(got.Tracks), len(original.Tracks), "tracks count")

	for i, tr := range got.Tracks {
		assertEqual(t, tr.Type, original.Tracks[i].Type, "track type")
		assertEqual(t, tr.Codec, original.Tracks[i].Codec, "track codec")
		assertEqual(t, tr.Language, original.Tracks[i].Language, "track lang")
	}
	assertEqual(t, len(got.Chapters), len(original.Chapters), "chapters count")
	assertEqual(t, len(got.Tags), len(original.Tags), "tags count")
}
