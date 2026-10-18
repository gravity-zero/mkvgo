package mp4

import (
	"context"
	"testing"
)

// TestRetimeKeepsAnEmptyEditAfterTheMedia: only the empty edits ahead of the media are the delay a retime moves; a pause after it stays where it is.
func TestRetimeKeepsAnEmptyEditAfterTheMedia(t *testing.T) {
	path := editFixture(t) // movie timescale 1000: segment durations in ms
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(i int, trak []byte) []byte {
			if i != 1 {
				return trak
			}
			tb, err := iterBoxes(trak)
			if err != nil {
				t.Fatal(err)
			}
			var w bw
			for _, b := range tb {
				switch b.typ {
				case "edts":
				case "tkhd":
					w.bytes(box(b.typ, b.payload))
					w.bytes(buildElstBox([]elstEntry{{segDur: 6000, mediaTime: 0, rate: 1 << 16}, {segDur: 1000, mediaTime: -1, rate: 1 << 16}}))
				default:
					w.bytes(box(b.typ, b.payload))
				}
			}
			return w.b
		})
	})
	if err := RetimeTracks(context.Background(), path, map[uint64]int64{2: 300_000_000}); err != nil {
		t.Fatal(err)
	}
	var got []elstEntry
	for _, b := range moovBoxesOf(t, path) {
		if b.typ != "trak" {
			continue
		}
		tb, _ := iterBoxes(b.payload)
		if id, _ := trakID(b.payload); id == 2 {
			got, _ = trakElstEntries(tb)
		}
	}
	want := []elstEntry{{segDur: 300, mediaTime: -1, rate: 1 << 16}, {segDur: 6000, mediaTime: 0, rate: 1 << 16}, {segDur: 1000, mediaTime: -1, rate: 1 << 16}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("edit list after a +300 ms retime: %+v, want %+v", got, want)
	}
}
