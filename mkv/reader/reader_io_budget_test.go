package reader

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
)

// ioCounter wraps the reader handed to Read and tallies the real I/O the parser
// drives against the underlying file: Read calls, Seek calls and bytes read.
// Read wraps it in a bufReadSeeker, so position queries served from the buffer
// cost nothing here, while every refill, jump seek and body-skip is counted —
// exactly the syscall budget that decides full-Read latency on a 9p/SMB mount.
type ioCounter struct {
	rs    io.ReadSeeker
	reads int
	seeks int
	bytes int64
}

func (c *ioCounter) Read(p []byte) (int, error) {
	n, err := c.rs.Read(p)
	c.reads++
	c.bytes += int64(n)
	return n, err
}

func (c *ioCounter) Seek(off int64, whence int) (int64, error) {
	c.seeks++
	return c.rs.Seek(off, whence)
}

const oneMiB = 1 << 20

// (a) SeekHead + tail Cues: the jump must skip the cluster region, so the seek
// count is identical for 20, 2 000 and 20 000 clusters and bytes stay tiny. The
// cluster size (2 KiB) keeps the region larger than fullReadBufSize even at 20
// clusters, so the buffer can never span head-to-Cues and mask the jump.
func TestIOBudget_SeekHeadTailCues_O1(t *testing.T) {
	const clusterSize = 2 << 10
	var prev *ioCounter
	for _, n := range []int{20, 2000, 20000} {
		cnt := &ioCounter{rs: bytes.NewReader(buildTailMKV(t, true, n, clusterSize, false))}
		c, err := Read(context.Background(), cnt, "x.mkv")
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		assertTailParsed(t, c)
		if cnt.bytes > oneMiB {
			t.Errorf("n=%d read %d bytes (> 1 MiB): the jump did not skip the cluster region", n, cnt.bytes)
		}
		if prev != nil && cnt.seeks != prev.seeks {
			t.Errorf("seeks scale with cluster count (%d at n=%d vs %d before): the SeekHead jump is not firing", cnt.seeks, n, prev.seeks)
		}
		t.Logf("(a) n=%5d: reads=%d seeks=%d bytes=%d", n, cnt.reads, cnt.seeks, cnt.bytes)
		prev = cnt
	}
}

// (b) SeekHead + Cues before the clusters (Doctor Strange layout): stop-early
// must halt at the first cluster, so the budget is O(1) — identical for 20 and
// 20 000 clusters.
func TestIOBudget_SeekHeadHeadCues_StopEarlyO1(t *testing.T) {
	const clusterSize = 2 << 10
	var prev *ioCounter
	for _, n := range []int{20, 20000} {
		cnt := &ioCounter{rs: bytes.NewReader(buildHeadCuesMKV(t, n, clusterSize))}
		c, err := Read(context.Background(), cnt, "x.mkv")
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(c.Cues) != tailCueCount || len(c.Tracks) != 2 {
			t.Fatalf("n=%d: head metadata not parsed: cues=%d tracks=%d", n, len(c.Cues), len(c.Tracks))
		}
		if cnt.bytes > oneMiB {
			t.Errorf("n=%d read %d bytes (> 1 MiB): stop-early did not halt at the first cluster", n, cnt.bytes)
		}
		if prev != nil && cnt.seeks != prev.seeks {
			t.Errorf("stop-early not O(1): %d seeks at n=%d vs %d before", cnt.seeks, n, prev.seeks)
		}
		t.Logf("(b) n=%5d: reads=%d seeks=%d bytes=%d", n, cnt.reads, cnt.seeks, cnt.bytes)
		prev = cnt
	}
}

// (c) No SeekHead: the walk must cross every cluster, so seeks scale with the
// cluster count (unavoidable). The point under test is that bytes do NOT — the
// 33 KiB clusters (> fullReadBufSize) would each trigger a buffer refill if the
// walk were buffered, so a regression would read ~32 KiB × clusters. The
// unbuffered walk keeps bytes well under 1 MiB at any cluster count.
func TestIOBudget_NoSeekHead_WalkNoAmplification(t *testing.T) {
	const clusterSize = 33 << 10
	small := &ioCounter{rs: bytes.NewReader(buildTailMKV(t, false, 20, clusterSize, false))}
	cs, err := Read(context.Background(), small, "s.mkv")
	if err != nil {
		t.Fatalf("20 clusters: %v", err)
	}
	large := &ioCounter{rs: bytes.NewReader(buildTailMKV(t, false, 2000, clusterSize, false))}
	cl, err := Read(context.Background(), large, "l.mkv")
	if err != nil {
		t.Fatalf("2000 clusters: %v", err)
	}

	assertTailParsed(t, cs)
	assertTailParsed(t, cl)

	if large.seeks <= small.seeks {
		t.Errorf("a no-SeekHead walk should seek more with more clusters: 20=%d 2000=%d", small.seeks, large.seeks)
	}
	if small.bytes > oneMiB || large.bytes > oneMiB {
		t.Errorf("buffer amplification on the fallback walk: bytes 20cl=%d 2000cl=%d (want < 1 MiB; a per-cluster refill would read ~%d)",
			small.bytes, large.bytes, int64(2000)*fullReadBufSize)
	}
	t.Logf("(c) no-seekhead: 20cl reads=%d seeks=%d bytes=%d | 2000cl reads=%d seeks=%d bytes=%d",
		small.reads, small.seeks, small.bytes, large.reads, large.seeks, large.bytes)
}

// (point 5) The optimized full Read must return a Container byte-identical to the
// unoptimized walk. Read the same logical content with a SeekHead (jump path)
// and without (walk path); every field a consumer relies on must match exactly.
func TestFullReadJumpWalkContainerParity(t *testing.T) {
	jump, err := Read(context.Background(), bytes.NewReader(buildTailMKV(t, true, 16, 64<<10, false)), "a.mkv")
	if err != nil {
		t.Fatalf("jump path: %v", err)
	}
	walk, err := Read(context.Background(), bytes.NewReader(buildTailMKV(t, false, 16, 64<<10, false)), "b.mkv")
	if err != nil {
		t.Fatalf("walk path: %v", err)
	}

	// Path is the only field expected to differ.
	jump.Path, walk.Path = "", ""
	if !reflect.DeepEqual(jump, walk) {
		t.Errorf("jump and walk produced different Containers:\n jump = %+v\n walk = %+v", jump, walk)
	}
}
