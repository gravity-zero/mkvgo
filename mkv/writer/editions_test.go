package writer

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestWriteKeepsOtherEditions: a rewrite keeps every chapter edition apart, the
// one Chapters holds marked as the default, not mixed into a single list.
func TestWriteKeepsOtherEditions(t *testing.T) {
	c := &mkv.Container{
		Info:          mkv.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks:        []mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "aac"}},
		Chapters:      []mkv.Chapter{{ID: 1, StartMs: 0}, {ID: 2, StartMs: 1000}, {ID: 3, StartMs: 2000}},
		OtherEditions: []mkv.Edition{{UID: 7, Hidden: true, Chapters: []mkv.Chapter{{ID: 4, StartMs: 0}, {ID: 5, StartMs: 500}}}},
	}
	var buf bytes.Buffer
	if err := Write(&buf, c); err != nil {
		t.Fatal(err)
	}
	got, err := reader.Read(context.Background(), bytes.NewReader(buf.Bytes()), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chapters) != 3 || len(got.OtherEditions) != 1 || len(got.OtherEditions[0].Chapters) != 2 ||
		got.OtherEditions[0].UID != 7 || !got.OtherEditions[0].Hidden {
		t.Errorf("read back %d chapters and editions %+v; want 3 and one hidden edition 7 of 2", len(got.Chapters), got.OtherEditions)
	}
}
