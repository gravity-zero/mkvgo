package mp4

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// goldenSegmentName is the video rendition's segment file name RemuxToHLS
// writes for the (0-based) n-th segment.
func goldenSegmentName(n int) string { return fmt.Sprintf("seg%05d.m4s", n+1) }

// clusterRange is one Cluster element's absolute byte range [start, end) in a
// built fixture - start is the element header's first byte, end is one past
// its last declared body byte.
type clusterRange struct{ start, end int64 }

// scanAllClusters walks a complete, known-size-clustered fixture (as buildMKV
// produces) and returns every Cluster's byte range, in file order - the test's
// own oracle for choosing meaningful growth prefixes (mid-cluster, cluster
// boundary, ...), independent of the growing plan's own scanner.
func scanAllClusters(t *testing.T, data []byte) []clusterRange {
	t.Helper()
	r := bytes.NewReader(data)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("EBML header: %v", err)
	}
	pos := int64(n) + h.Size
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	h2, n2, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatalf("Segment header: %v", err)
	}
	if h2.ID != mkv.IDSegment {
		t.Fatalf("expected Segment, got 0x%X", h2.ID)
	}
	p := pos + int64(n2)
	var ranges []clusterRange
	for {
		if _, err := r.Seek(p, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		eh, en, err := ebml.ReadElementHeader(r)
		if err != nil {
			break
		}
		if eh.ID != mkv.IDCluster {
			if len(ranges) > 0 {
				break // a trailing element (e.g. Cues) after the last cluster
			}
			if eh.Size < 0 {
				t.Fatalf("unexpected unknown-size element 0x%X before the first cluster", eh.ID)
			}
			p += int64(en) + eh.Size
			continue // Void/SeekHead/Info/Tracks/... before the first cluster
		}
		bodyEnd := p + int64(en) + eh.Size
		ranges = append(ranges, clusterRange{p, bodyEnd})
		p = bodyEnd
	}
	if len(ranges) < 3 {
		t.Fatalf("fixture has only %d clusters, need several for a meaningful growth test", len(ranges))
	}
	return ranges
}

// buildGrowingFixture writes a synthetic source with several short clusters
// (so mid-cluster growth stages are easy to hit) and returns its complete bytes.
func buildGrowingFixture(t *testing.T) []byte {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 48000.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 250; i++ { // video, 40ms frames, keyframe every 1s -> 10s
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 500; i++ { // audio, 20ms frames
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
			data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		gblocks)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// buildGoldenHLS packages the complete fixture with RemuxToHLS: the golden
// this test compares every growing-plan output against, byte for byte.
func buildGoldenHLS(t *testing.T, data []byte) (dir string, numSegs int) {
	t.Helper()
	dir = t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "golden")
	if err := RemuxToHLS(context.Background(), src, out, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	segs, _ := filepath.Glob(filepath.Join(out, "seg0*.m4s"))
	return out, len(segs)
}

// TestGrowingHLSByteIdentity is the hard test: a source is fed to a
// GrowingHLSPlan in increasing prefixes (head only, mid-cluster, whole
// clusters, complete file) via MemFS, Refresh-ing after each growth. Every
// segment served at every stage must be byte-identical to the golden
// RemuxToHLS output of the same index, and the init segment must be
// byte-identical from the very first stage a rendition is servable.
func TestGrowingHLSByteIdentity(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	goldenDir, goldenSegs := buildGoldenHLS(t, data)

	m := mkv.NewMemFS()
	fs := m.FS()
	const path = "in.mkv"

	// Stage 0: only the head (no cluster at all yet).
	m.Put(path, data[:clusters[0].start])
	plan, err := PlanGrowingHLS(context.Background(), path, Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatalf("PlanGrowingHLS at head-only stage: %v", err)
	}
	if n := plan.NumSegments(); n != 0 {
		t.Fatalf("head-only stage: NumSegments = %d, want 0", n)
	}

	checkPublished := func(stage string) {
		t.Helper()
		n := plan.NumSegments()
		if n > goldenSegs {
			t.Fatalf("%s: NumSegments = %d exceeds golden's %d", stage, n, goldenSegs)
		}
		for k := 0; k < n; k++ {
			got, err := plan.Segment(context.Background(), k)
			if err != nil {
				t.Fatalf("%s: Segment(%d): %v", stage, k, err)
			}
			want, err := os.ReadFile(filepath.Join(goldenDir, goldenSegmentName(k)))
			if err != nil {
				t.Fatalf("%s: read golden segment %d: %v", stage, k, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: segment %d differs from golden (%d vs %d bytes)", stage, k, len(got), len(want))
			}
		}
	}

	// Grow one cluster at a time up to (but not including) the last one, and
	// once more with a MID-cluster cut inserted between two whole clusters.
	for i, cr := range clusters {
		if i == 0 {
			continue // already covered by the head-only stage
		}
		// Mid-cluster cut: half of this cluster's bytes are present. No segment
		// that would need this cluster's data may appear yet.
		mid := cr.start + (cr.end-cr.start)/2
		m.Put(path, data[:mid])
		before := plan.NumSegments()
		if _, err := plan.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh mid-cluster %d: %v", i, err)
		}
		if plan.NumSegments() != before {
			t.Fatalf("mid-cluster %d: NumSegments changed from %d to %d on a partial trailing cluster",
				i, before, plan.NumSegments())
		}
		checkPublished("mid-cluster")

		// Now the whole cluster.
		m.Put(path, data[:cr.end])
		if _, err := plan.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh whole cluster %d: %v", i, err)
		}
		checkPublished("whole-cluster")
	}

	// Complete file (carries Cues -> auto-detected finalization).
	m.Put(path, data)
	if _, err := plan.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh at completion: %v", err)
	}
	if plan.NumSegments() != goldenSegs {
		t.Fatalf("final NumSegments = %d, want %d (golden)", plan.NumSegments(), goldenSegs)
	}
	checkPublished("final")

	wantInit, err := os.ReadFile(filepath.Join(goldenDir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.InitSegment(); !bytes.Equal(got, wantInit) {
		t.Errorf("finalized init.mp4 differs from golden (%d vs %d bytes)", len(got), len(wantInit))
	}
}

// TestGrowingHLSPartialClusterHeld grows the file to a point strictly inside a
// cluster and asserts no segment needing that cluster is published; once the
// cluster completes, the segment appears and is byte-identical to golden.
func TestGrowingHLSPartialClusterHeld(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	goldenDir, _ := buildGoldenHLS(t, data)

	m := mkv.NewMemFS()
	fs := m.FS()
	const path = "in.mkv"

	// Grow to include every cluster up to (not including) the 4th, whole.
	cut := clusters[3]
	m.Put(path, data[:cut.start])
	plan, err := PlanGrowingHLS(context.Background(), path, Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	before := plan.NumSegments()

	// Now land in the MIDDLE of cluster 4.
	mid := cut.start + (cut.end-cut.start)/2
	m.Put(path, data[:mid])
	if _, err := plan.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() != before {
		t.Fatalf("a partial trailing cluster changed NumSegments from %d to %d", before, plan.NumSegments())
	}

	// Complete the cluster: the boundary it may carry becomes visible, closing
	// whatever segment was still open.
	m.Put(path, data[:cut.end])
	if _, err := plan.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := plan.NumSegments()
	if after < before {
		t.Fatalf("NumSegments went backwards: %d -> %d", before, after)
	}
	for k := before; k < after; k++ {
		got, err := plan.Segment(context.Background(), k)
		if err != nil {
			t.Fatalf("Segment(%d): %v", k, err)
		}
		want, err := os.ReadFile(filepath.Join(goldenDir, goldenSegmentName(k)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("segment %d differs from golden right after its cluster completed", k)
		}
	}
}

// TestGrowingHLSPlaylistLifecycle checks EVENT/no-ENDLIST while growing, VOD+
// ENDLIST once Complete() is called, and auto-detected finalization from a
// Cues-bearing complete file presented via Refresh.
func TestGrowingHLSPlaylistLifecycle(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)

	t.Run("Complete", func(t *testing.T) {
		m := mkv.NewMemFS()
		fs := m.FS()
		m.Put("in.mkv", data[:clusters[2].end])
		plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		pl := plan.MediaPlaylist()
		if pl == nil {
			t.Fatal("no media playlist yet")
		}
		if !bytes.Contains(pl, []byte("#EXT-X-PLAYLIST-TYPE:EVENT")) {
			t.Errorf("growing playlist must be EVENT-typed:\n%s", pl)
		}
		if bytes.Contains(pl, []byte("#EXT-X-ENDLIST")) {
			t.Errorf("growing playlist must not carry ENDLIST yet:\n%s", pl)
		}
		before := plan.NumSegments()

		// Grow the rest and Complete().
		m.Put("in.mkv", data)
		plan.Complete()
		pl = plan.MediaPlaylist()
		if !bytes.Contains(pl, []byte("#EXT-X-PLAYLIST-TYPE:VOD")) || !bytes.Contains(pl, []byte("#EXT-X-ENDLIST")) {
			t.Errorf("finalized playlist must be VOD+ENDLIST:\n%s", pl)
		}
		if plan.NumSegments() < before {
			t.Errorf("segment count went backwards after Complete: %d -> %d", before, plan.NumSegments())
		}
	})

	t.Run("AutoDetect", func(t *testing.T) {
		m := mkv.NewMemFS()
		fs := m.FS()
		m.Put("in.mkv", data[:clusters[2].end])
		plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
		if err != nil {
			t.Fatal(err)
		}
		before := plan.NumSegments()
		// Present the whole (Cues-bearing) file through an ordinary Refresh -
		// no explicit Complete() call - and expect finalization on its own.
		m.Put("in.mkv", data)
		if _, err := plan.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		pl := plan.MediaPlaylist()
		if !bytes.Contains(pl, []byte("#EXT-X-ENDLIST")) {
			t.Errorf("Refresh over a complete, Cues-bearing file must auto-finalize:\n%s", pl)
		}
		if plan.NumSegments() < before {
			t.Errorf("segment count went backwards: %d -> %d", before, plan.NumSegments())
		}
	})
}

// TestGrowingHLSStableNumbering checks that a published segment's byte range
// (its content) never changes across later Refreshes, growth, or completion.
func TestGrowingHLSStableNumbering(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data[:clusters[2].end])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() == 0 {
		t.Fatal("expected at least one closed segment by cluster 3")
	}
	first, err := plan.Segment(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	m.Put("in.mkv", data[:clusters[4].end])
	if _, err := plan.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.Put("in.mkv", data)
	if _, err := plan.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	plan.Complete()

	again, err := plan.Segment(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Errorf("segment 0's bytes changed across growth/finalization")
	}
}

// TestGrowingHLSRefusesEncryption checks Options.Encrypt/CENC are refused
// explicitly, not silently mishandled.
func TestGrowingHLSRefusesEncryption(t *testing.T) {
	data := buildGrowingFixture(t)
	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data)

	if _, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, Encrypt: &HLSEncryption{}}); err == nil {
		t.Error("Options.Encrypt must be refused for a growing plan")
	}
	if _, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, CENC: &CENCOptions{}}); err == nil {
		t.Error("Options.CENC must be refused for a growing plan")
	}
}

// TestGrowingHLSConcurrency interleaves Refresh and Resource calls: no crash,
// no corruption of the segment list, and the final state matches full growth.
func TestGrowingHLSConcurrency(t *testing.T) {
	data := buildGrowingFixture(t)
	clusters := scanAllClusters(t, data)
	_, goldenSegs := buildGoldenHLS(t, data)

	m := mkv.NewMemFS()
	fs := m.FS()
	m.Put("in.mkv", data[:clusters[0].start])
	plan, err := PlanGrowingHLS(context.Background(), "in.mkv", Options{FS: fs, SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _, _ = plan.Resource(context.Background(), "master.m3u8")
				n := plan.NumSegments()
				for k := 0; k < n; k++ {
					_, _ = plan.Segment(context.Background(), k)
				}
			}
		}()
	}

	for _, cr := range clusters {
		m.Put("in.mkv", data[:cr.end])
		if _, err := plan.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	m.Put("in.mkv", data)
	plan.Complete()
	close(stop)
	wg.Wait()

	if plan.NumSegments() != goldenSegs {
		t.Errorf("final NumSegments = %d, want %d", plan.NumSegments(), goldenSegs)
	}
}
