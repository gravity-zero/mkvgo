package main

import "testing"

// TestNextBlobWindow: a probe's first Blob read is small (a 512 KiB read cost
// more than the probe itself), sequential reads double up to the cap, and a
// jump starts small again.
func TestNextBlobWindow(t *testing.T) {
	steps := []struct {
		prevEnd, off int64
		prev, want   int
	}{
		{0, 0, 0, 64 << 10},
		{64 << 10, 64 << 10, 64 << 10, 128 << 10},
		{192 << 10, 192 << 10, 128 << 10, 256 << 10},
		{448 << 10, 448 << 10, 256 << 10, 512 << 10},
		{960 << 10, 960 << 10, 512 << 10, 512 << 10},
		{960 << 10, 50 << 20, 512 << 10, 64 << 10},
	}
	for _, s := range steps {
		if got := nextBlobWindow(s.prevEnd, s.off, s.prev); got != s.want {
			t.Errorf("nextBlobWindow(%d, %d, %d) = %d, want %d", s.prevEnd, s.off, s.prev, got, s.want)
		}
	}
}
