package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func tracksOfAllReaders(t *testing.T, data []byte) map[string][]mkv.Track {
	t.Helper()
	got := map[string][]mkv.Track{}
	if c, err := Read(context.Background(), bytes.NewReader(data), "t.mkv"); err == nil {
		got["Read"] = c.Tracks
	}
	if c, err := ReadMeta(context.Background(), bytes.NewReader(data), "t.mkv"); err == nil {
		got["ReadMeta"] = c.Tracks
	}
	if c, _, err := ReadStream(context.Background(), bytes.NewReader(data)); err == nil {
		got["ReadStream"] = c.Tracks
	}
	return got
}

func TestTrackEntryWithoutNumberIsNotATrack(t *testing.T) {
	audio := trackEntry(
		uintElem(mkv.IDTrackNumber, 2, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeAudio, 1),
		strElem(mkv.IDCodecID, "A_OPUS"),
	)
	zero := trackEntry(
		uintElem(mkv.IDTrackNumber, 0, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_VP9"),
	)
	absent := trackEntry(
		uintElem(mkv.IDTrackType, mkv.TrackTypeSubtitle, 1),
		strElem(mkv.IDCodecID, "S_TEXT/UTF8"),
	)
	var seg bytes.Buffer
	seg.Write(masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4)))
	seg.Write(masterElem(mkv.IDTracks, zero, audio, absent))
	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())

	got := tracksOfAllReaders(t, buf.Bytes())
	if len(got) != 3 {
		t.Fatalf("readers that accepted the file: %d, want 3", len(got))
	}
	for name, tracks := range got {
		if len(tracks) != 1 || tracks[0].ID != 2 || tracks[0].Codec != "opus" {
			t.Errorf("%s: %d tracks, want only track 2 opus", name, len(tracks))
		}
	}
}

func TestTrackFoundInJunkWithoutNumberIsNotPublished(t *testing.T) {
	data := []byte("\x1aEߣ\x80\x18S\x80g000Ȉ0\x16T\xaek\x90\xae\x8e\xff\xff")
	for name, tracks := range tracksOfAllReaders(t, data) {
		for _, tr := range tracks {
			if tr.ID == 0 {
				t.Errorf("%s publishes a track without a TrackNumber (type %q, codec %q)", name, tr.Type, tr.Codec)
			}
		}
	}
}
