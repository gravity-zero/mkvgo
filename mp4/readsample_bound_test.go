package mp4

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// sampleReadCounter counts the bytes read through it.
type sampleReadCounter struct {
	io.ReadSeeker
	n int
}

func (c *sampleReadCounter) Read(p []byte) (int, error) {
	n, err := c.ReadSeeker.Read(p)
	c.n += n
	return n, err
}

// TestReadSampleMaxRefusesBeforeReading: a sample declared past its bound is refused without a byte read.
func TestReadSampleMaxRefusesBeforeReading(t *testing.T) {
	r := &sampleReadCounter{ReadSeeker: bytes.NewReader(make([]byte, 2<<20))}
	_, err := readSampleMax(r, 0, 2<<20, maxSubtitleSampleBytes, "subtitle")
	if err == nil || !strings.Contains(err.Error(), "damaged") || r.n != 0 {
		t.Errorf("err = %v after %d bytes read, want a refusal before reading", err, r.n)
	}
	if b, err := readSampleMax(r, 0, 100, maxSubtitleSampleBytes, "subtitle"); err != nil || len(b) != 100 {
		t.Errorf("a sample within its bound: %d bytes, %v", len(b), err)
	}
}
