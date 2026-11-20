package reader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
)

// cancelAfter cancels its context once the source was read past a given offset, and reads 32 bytes at a time.
type cancelAfter struct {
	*bytes.Reader
	past   int64
	cancel context.CancelFunc
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if pos, _ := c.Reader.Seek(0, io.SeekCurrent); pos >= c.past {
		c.cancel()
	}
	return c.Reader.Read(p[:min(len(p), 32)]) // short reads, so the walk meets the cancellation on its way
}

// TestKeyframeWalkStopsOnCancel: the keyframe walk reads the whole file; a context cancelled on the way ended in a
// read that returned no error and the keyframes met so far, as if they were the file's.
func TestKeyframeWalkStopsOnCancel(t *testing.T) {
	data := livefixture.Build(livefixture.Options{SizedClusters: true})
	whole, err := ReadMeta(context.Background(), bytes.NewReader(data), "", WithKeyframeIndex())
	if err != nil || len(whole.Keyframes) < 2 {
		t.Fatalf("fixture: %d keyframes, err %v", len(whole.Keyframes), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := int64(bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75}))
	c, err := ReadMeta(ctx, &cancelAfter{Reader: bytes.NewReader(data), past: first + 8, cancel: cancel}, "", WithKeyframeIndex())
	if !errors.Is(err, context.Canceled) {
		n := -1
		if c != nil {
			n = len(c.Keyframes)
		}
		t.Fatalf("err = %v with %d keyframes of %d; want context.Canceled", err, n, len(whole.Keyframes))
	}
}
