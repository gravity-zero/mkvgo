package reader

import (
	"bytes"
	"context"
	"testing"
)

// TestReadTruncatedTail covers a file cut mid-element after the head metadata:
// the Segment walk hits an unexpected EOF reading the truncated tail (here the
// Cues), but Info + Tracks are already in hand, so the read must return them
// rather than failing - the behaviour external probers show on a truncated Matroska.
func TestReadTruncatedTail(t *testing.T) {
	full := segmentMKV(infoElem(), tracksElem(), cuesElem(50))
	trunc := full[:len(full)-100] // cut into the Cues body (header stays intact)

	t.Run("Read", func(t *testing.T) {
		c, err := Read(context.Background(), bytes.NewReader(trunc), "x.mkv")
		if err != nil {
			t.Fatalf("truncated tail must not fail the read: %v", err)
		}
		if len(c.Tracks) != 2 {
			t.Errorf("tracks = %d, want 2 from the truncated file", len(c.Tracks))
		}
	})

	t.Run("ReadMeta", func(t *testing.T) {
		c, err := ReadMeta(context.Background(), bytes.NewReader(trunc), "x.mkv")
		if err != nil {
			t.Fatalf("truncated tail must not fail ReadMeta: %v", err)
		}
		if len(c.Tracks) != 2 {
			t.Errorf("tracks = %d, want 2", len(c.Tracks))
		}
	})

	// A complete file still parses its Cues into a keyframe index (control: the
	// tolerance must not mask a non-truncated read).
	c, err := Read(context.Background(), bytes.NewReader(full), "x.mkv")
	if err != nil {
		t.Fatalf("complete file: %v", err)
	}
	if len(c.Keyframes) == 0 {
		t.Error("complete file should still yield a keyframe index")
	}
}
