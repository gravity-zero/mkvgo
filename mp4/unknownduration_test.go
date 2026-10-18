package mp4

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"
)

// TestJoinTakesAnUnknownMovieDurationFromTheTracks: a recorder's mvhd duration 0xFFFFFFFF means unset, not 49 days.
func TestJoinTakesAnUnknownMovieDurationFromTheTracks(t *testing.T) {
	a, b := editFixture(t), editFixture(t)
	rewriteTailMoov(t, a, func(p []byte) []byte {
		boxes, err := iterBoxes(p)
		if err != nil {
			t.Fatal(err)
		}
		var w bw
		for _, bx := range boxes {
			if bx.typ == "mvhd" {
				bx.payload = append([]byte(nil), bx.payload...)
				binary.BigEndian.PutUint32(bx.payload[16:20], 0xFFFFFFFF) // version 0 duration
			}
			w.bytes(box(bx.typ, bx.payload))
		}
		return w.b
	})
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), []string{a, b}, dst); err != nil {
		t.Fatalf("join after an unknown movie duration: %v", err)
	}
	if got := mustMeta(t, dst).DurationMs; got < 11900 || got > 12100 {
		t.Errorf("joined movie lasts %d ms, want two 6 s files", got)
	}
}
