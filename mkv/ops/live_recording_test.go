package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// liveFile writes a live-recording fixture to a temp file and returns its path.
func liveFile(t *testing.T, o livefixture.Options) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live.mkv")
	writeAll(t, path, livefixture.Build(o))
	return path
}

// checkLiveOutput asserts dst is a sealed, indexed copy holding every track
// and every block of a live fixture.
func checkLiveOutput(t *testing.T, dst string) {
	t.Helper()
	c, err := reader.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if len(c.Tracks) != livefixture.Tracks {
		t.Fatalf("output tracks = %d, want %d (the blocks would belong to no track)", len(c.Tracks), livefixture.Tracks)
	}
	if len(c.Cues) == 0 {
		t.Error("output has no Cues")
	}
	if n := blockReaderIteratesCleanly(t, dst); n != livefixture.Blocks {
		t.Errorf("output blocks = %d, want %d", n, livefixture.Blocks)
	}
}

// TestReindexLiveRecording: the strict reindex bounds each unknown-size
// Cluster by walking its children (it used to refuse the file), seals the
// sizes, and its rollback delta still rebuilds the source byte for byte -
// unknown-size headers included, at either width.
func TestReindexLiveRecording(t *testing.T) {
	for name, o := range map[string]livefixture.Options{
		"8-byte unknown sizes":        {},
		"1-byte unknown sizes":        {ShortUnknown: true},
		"Tags after the last cluster": {TailTags: true},
		"sized Segment":               {SizedSegment: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := liveFile(t, o)
			dir := t.TempDir()
			dst := filepath.Join(dir, "out.mkv")
			var delta bytes.Buffer
			if err := Reindex(ctx, src, dst, mkv.Options{DeepVerify: true, RollbackSink: &delta}); err != nil {
				t.Fatalf("Reindex: %v", err)
			}
			checkLiveOutput(t, dst)
			if c, _ := reader.Open(ctx, dst); o.TailTags != (c.Tags != nil) {
				t.Errorf("tail Tags kept = %v, want %v", c.Tags != nil, o.TailTags)
			}

			restored := filepath.Join(dir, "restored.mkv")
			if err := ApplyRollback(ctx, dst, &delta, restored); err != nil {
				t.Fatalf("ApplyRollback: %v", err)
			}
			got, _ := os.ReadFile(restored)
			if !bytes.Equal(got, livefixture.Build(o)) {
				t.Error("rollback did not rebuild the source byte for byte")
			}
		})
	}
}

// TestReindexLiveRecordingBrokenChain: an unknown-size Cluster whose children
// stop parsing before any boundary is corruption, not the end of the cluster.
// The strict walk keeps refusing it and points at the opt-in.
func TestReindexLiveRecordingBrokenChain(t *testing.T) {
	data := livefixture.Build(livefixture.Options{})
	data = data[:len(data)-3] // the last block is cut mid-payload
	dir := t.TempDir()
	src := filepath.Join(dir, "cut.mkv")
	writeAll(t, src, data)

	err := Reindex(context.Background(), src, filepath.Join(dir, "out.mkv"))
	if !errors.Is(err, ErrCorruptSource) {
		t.Fatalf("Reindex on a cut cluster = %v, want ErrCorruptSource", err)
	}
}

// TestResyncKeepsMetadataBehindHeadJunk: junk ahead of the metadata used to
// send the tolerant walk to the first CLUSTER, taking Info and Tracks into the
// skipped range - exit 0, "recovered ~100%", and an output whose blocks belong
// to no track. The skipped range must be the junk and nothing else.
func TestResyncKeepsMetadataBehindHeadJunk(t *testing.T) {
	for name, o := range map[string]livefixture.Options{
		"junk after the Segment header": {JunkHead: 134, ShortUnknown: true},
		"junk between Info and Tracks":  {JunkMid: 134},
		"junk and tail Tags":            {JunkHead: 134, TailTags: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			src := liveFile(t, o)
			data := livefixture.Build(o)
			junkAt := int64(bytes.Index(data, bytes.Repeat([]byte{livefixture.JunkByte}, 134)))

			// Strict contract unchanged: undecodable bytes are refused.
			if err := Reindex(ctx, src, filepath.Join(t.TempDir(), "strict.mkv")); !errors.Is(err, ErrCorruptSource) {
				t.Fatalf("strict Reindex = %v, want ErrCorruptSource", err)
			}

			var skipped []mkv.DamagedRange
			dst := filepath.Join(t.TempDir(), "resync.mkv")
			err := Reindex(ctx, src, dst, mkv.Options{
				Resync: true,
				OnSkip: func(r mkv.DamagedRange) { skipped = append(skipped, r) },
			})
			if err != nil {
				t.Fatalf("Reindex with Resync: %v", err)
			}
			if len(skipped) != 1 || skipped[0].StartOffset != junkAt || skipped[0].EndOffset != junkAt+134 {
				t.Errorf("skipped = %+v, want exactly the junk [%d,%d)", skipped, junkAt, junkAt+134)
			}
			checkLiveOutput(t, dst)

			report, err := MapDamage(ctx, src)
			if err != nil {
				t.Fatalf("MapDamage: %v", err)
			}
			if report.BytesSkipped != 134 {
				t.Errorf("MapDamage skipped %d bytes, want the 134 junk bytes", report.BytesSkipped)
			}
		})
	}
}

// TestResyncOnLiveRecordingReportsNoRepair: an unknown-size Cluster is how a
// live recording is written, not damage. The tolerant walk used to send every
// one of them through the surgical recovery and report each as a repaired
// region ("media kept that a plain resync would have dropped") - 36 repairs on
// a healthy 36-cluster file. A clean live source now takes the same path as
// the strict reindex: identical output, nothing reported.
func TestResyncOnLiveRecordingReportsNoRepair(t *testing.T) {
	ctx := context.Background()
	src := liveFile(t, livefixture.Options{TailTags: true})
	dir := t.TempDir()
	strict, tolerant := filepath.Join(dir, "strict.mkv"), filepath.Join(dir, "resync.mkv")
	if err := Reindex(ctx, src, strict); err != nil {
		t.Fatalf("strict Reindex: %v", err)
	}
	var skips, repairs int
	err := Reindex(ctx, src, tolerant, mkv.Options{
		Resync:   true,
		OnSkip:   func(mkv.DamagedRange) { skips++ },
		OnRepair: func(mkv.RepairedRange) { repairs++ },
	})
	if err != nil {
		t.Fatalf("Reindex with Resync: %v", err)
	}
	if skips != 0 || repairs != 0 {
		t.Errorf("clean live source: %d skips, %d repairs reported, want none", skips, repairs)
	}
	a, _ := os.ReadFile(strict)
	b, _ := os.ReadFile(tolerant)
	if !bytes.Equal(a, b) {
		t.Error("Resync output differs from the strict one on a clean live source")
	}
}

// TestTrackEndsBehindHeadJunk: the tail walk reads blocks from the start of a
// file that has neither Duration nor Cues; head junk used to leave every track
// "never seen".
func TestTrackEndsBehindHeadJunk(t *testing.T) {
	report, err := TrackEnds(context.Background(), liveFile(t, livefixture.Options{JunkHead: 134}))
	if err != nil {
		t.Fatalf("TrackEnds: %v", err)
	}
	if len(report.Ends) != livefixture.Tracks {
		t.Fatalf("ends = %d tracks, want %d", len(report.Ends), livefixture.Tracks)
	}
	for _, e := range report.Ends {
		if e.EndMs < livefixture.LastBlockMs {
			t.Errorf("track %d ends at %d ms, want at least the last block (%d ms)", e.Track, e.EndMs, livefixture.LastBlockMs)
		}
	}
}
