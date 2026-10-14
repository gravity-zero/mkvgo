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

// TestBlobCacheTwoRegions: reads that alternate between two regions of a Blob
// (an MP4 whose video and audio chunks are not interleaved) keep a window on
// each, so each region is read about once instead of once per jump.
func TestBlobCacheTwoRegions(t *testing.T) {
	const size = 200 << 20
	var c blobCache
	fetches, fetched := 0, 0
	fetch := func(dst []byte, off int64) ([]byte, error) {
		fetches++
		fetched += len(dst)
		return dst, nil
	}
	video, audio := int64(7<<20), int64(107<<20)
	buf := make([]byte, 30000)
	for i := 0; i < 400; i++ { // 400 alternations: 30 KB of video, then 700 bytes of audio
		for _, r := range []struct {
			pos *int64
			n   int
		}{{&video, 30000}, {&audio, 700}} {
			for done := 0; done < r.n; {
				n, err := c.read(buf[done:r.n], *r.pos, size, fetch)
				if err != nil {
					t.Fatal(err)
				}
				done += n
				*r.pos += int64(n)
			}
		}
	}
	read := 400 * (30000 + 700)
	if fetched > 2*read || fetches > 60 {
		t.Errorf("%d fetches, %d bytes fetched for %d bytes read: want each region read about once", fetches, fetched, read)
	}
}

// TestBlobCacheHolds: holds tells a range inside a window already read from one
// that would cost a Blob read, so the frame-rate estimate reads nothing more.
func TestBlobCacheHolds(t *testing.T) {
	var c blobCache
	fetch := func(dst []byte, off int64) ([]byte, error) { return dst, nil }
	if c.holds(0, 1) {
		t.Fatal("an empty cache holds nothing")
	}
	if _, err := c.read(make([]byte, 16), 0, 1<<20, fetch); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		off, n int64
		want   bool
	}{{0, 32, true}, {64<<10 - 32, 32, true}, {64<<10 - 16, 32, false}, {64 << 10, 1, false}} {
		if got := c.holds(r.off, r.n); got != r.want {
			t.Errorf("holds(%d, %d) = %v, want %v", r.off, r.n, got, r.want)
		}
	}
}
