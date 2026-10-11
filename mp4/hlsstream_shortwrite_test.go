package mp4

import (
	"context"
	"errors"
	"io"
	"testing"
)

// A writer that takes fewer bytes than asked without an error is a short write, never a clean end: for a streamed segment and for bytes in hand.
func TestWriteToReportsShortWrite(t *testing.T) {
	src := openGOPSource(t, 3, false, false)
	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: openGOPMs})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"seg00001.m4s", "init.mp4"} {
		h, err := plan.Open(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		m, err := h.WriteTo(context.Background(), &halfWriter{})
		if !errors.Is(err, io.ErrShortWrite) || m >= h.Size() {
			t.Fatalf("%s: wrote %d of %d, err %v, want io.ErrShortWrite", name, m, h.Size(), err)
		}
	}
}

// halfWriter accepts half of every write and reports no error.
type halfWriter struct{}

func (halfWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }
