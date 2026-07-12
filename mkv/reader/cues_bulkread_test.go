package reader

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// callCountingReadSeeker counts the number of Read CALLS - a proxy for syscalls
// on a real *os.File - rather than bytes read. parseCues' bulk read does not
// change how many bytes cross the boundary (the whole Cues body is read either
// way); it collapses thousands of tiny per-CuePoint reads into one, which only
// shows up as a drop in call count. On a network FS each call is a round-trip,
// so this count is what governs whether a long file's index takes ms or minutes.
type callCountingReadSeeker struct {
	rs    io.ReadSeeker
	calls int
}

func (c *callCountingReadSeeker) Read(p []byte) (int, error) {
	c.calls++
	return c.rs.Read(p)
}

func (c *callCountingReadSeeker) Seek(off int64, whence int) (int64, error) {
	return c.rs.Seek(off, whence)
}

// TestParseCuesReadsBodyInBulk proves the Cues index is pulled in a single read
// instead of ~4+ tiny reads per CuePoint. Streaming the index point-by-point
// needs at least one Read per CuePoint just for its header, so with n CuePoints
// the old path issued >n Read calls; the bulk read keeps the entire file parse
// (header + info + tracks + one Cues read + a skipped cluster) far below n. The
// Cues are still parsed identically - the values must match.
func TestParseCuesReadsBodyInBulk(t *testing.T) {
	const n = 2000
	data := segmentMKV(infoElem(), tracksElem(), cuesElem(n), clusterElem())

	cr := &callCountingReadSeeker{rs: bytes.NewReader(data)}
	c, err := Read(context.Background(), cr, "x.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(c.Cues) != n {
		t.Fatalf("parsed %d cues, want %d", len(c.Cues), n)
	}
	// Spot-check the boundaries to be sure the in-memory parse is faithful.
	if c.Cues[0].TimeMs != 0 || c.Cues[0].ClusterPos != 0 {
		t.Errorf("first cue = %+v, want TimeMs=0 ClusterPos=0", c.Cues[0])
	}
	if got, want := c.Cues[n-1].TimeMs, int64((n-1)*1000); got != want {
		t.Errorf("last cue TimeMs = %d, want %d", got, want)
	}

	// Streaming the index would cost >= n Read calls (one per CuePoint header
	// minimum, ~4 in practice). The bulk read collapses that to one, so the whole
	// parse must stay well under n. The bound is deliberately generous toward the
	// small metadata elements while still failing loudly on a regression to
	// point-by-point reads.
	if cr.calls >= n {
		t.Errorf("Read issued %d Read calls for %d CuePoints; the Cues body must be read in bulk (well under %d)", cr.calls, n, n)
	}
	t.Logf("parsed %d CuePoints in %d Read calls (point-by-point streaming would be >%d)", n, cr.calls, n)
}
