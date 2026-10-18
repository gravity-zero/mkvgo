package mp4

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestGridWindowBytesStayAcrossRebuilds pins that a grid-timed audio segment built cold keeps its bytes when the
// window before it is built afterwards and it is built again: same name, same weak ETag, same bytes.
func TestGridWindowBytesStayAcrossRebuilds(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 300; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: iframeTestFrame(i, i%25 == 0)})
	}
	for i := 0; int64(i)*1024*1000/44100 < 12000; i++ {
		ms := int64(i) * 1024 * 1000 / 44100
		if ms >= 3700 && ms < 3950 {
			ms += 23 // a transient ahead of the grid, ending at the boundary
		}
		blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(blocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, DefaultDurationNs: 1024 * 1_000_000_000 / 44100},
	}, blocks)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	fts := plan.fts()
	cold, _, err := plan.Resource(ctx, renditionSegment(fts, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Resource(ctx, renditionSegment(fts, 1, 1)); err != nil {
		t.Fatal(err)
	}
	again, _, err := plan.Resource(ctx, renditionSegment(fts, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cold, again) {
		t.Errorf("%s changed between two builds (%d and %d bytes) under one ETag", renditionSegment(fts, 1, 2), len(cold), len(again))
	}
}
