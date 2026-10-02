package ops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestAdviceOnHeadJunkNamesTheCommandThatWorks: a remedy must never recommend
// what will be refused. On a file with junk ahead of its metadata the strict
// reindex is refused, so Diagnose, CueHealth and Validate must name the
// tolerant one - and that command must actually leave a file with nothing
// left to repair.
func TestAdviceOnHeadJunkNamesTheCommandThatWorks(t *testing.T) {
	ctx := context.Background()
	src := liveFile(t, livefixture.Options{JunkHead: 134, ShortUnknown: true})

	d, err := Diagnose(ctx, src)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var damaged bool
	for _, f := range d.Findings {
		damaged = damaged || f.Kind == "damaged"
		for _, advice := range []string{f.Detail, f.Remedy} {
			if strings.Contains(advice, "mkvgo reindex") && !strings.Contains(advice, "mkvgo reindex --resync") {
				t.Errorf("[%s] recommends the strict reindex, which this file is refused by: %q", f.Kind, advice)
			}
		}
	}
	if !damaged {
		t.Errorf("no damaged finding for 134 undecodable head bytes: %+v", d.Findings)
	}

	issues, err := Validate(ctx, src)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var warned bool
	for _, is := range issues {
		warned = warned || is.Code == "undecodable-bytes"
		if strings.Contains(is.Message, "`mkvgo reindex`") {
			t.Errorf("[%s] recommends the strict reindex: %q", is.Code, is.Message)
		}
	}
	if !warned {
		t.Error("Validate does not report the undecodable bytes")
	}

	// The strict command is indeed refused; the recommended one works, and
	// its output needs neither.
	if err := Reindex(ctx, src, filepath.Join(t.TempDir(), "strict.mkv")); err == nil {
		t.Fatal("strict Reindex accepted the junk: the advice under test would be moot")
	}
	dst := filepath.Join(t.TempDir(), "repaired.mkv")
	if err := Reindex(ctx, src, dst, mkv.Options{Resync: true}); err != nil {
		t.Fatalf("the recommended Reindex with Resync: %v", err)
	}
	after, err := Diagnose(ctx, dst)
	if err != nil {
		t.Fatalf("Diagnose after repair: %v", err)
	}
	if !after.Healthy {
		t.Errorf("after the recommended repair: %+v", after.Findings)
	}
	issues, err = Validate(ctx, dst)
	if err != nil {
		t.Fatalf("Validate after repair: %v", err)
	}
	for _, is := range issues {
		if is.Code == "undecodable-bytes" {
			t.Errorf("repaired file still reports %q", is.Message)
		}
	}
}

// TestAdviceOnCleanLiveRecordingKeepsTheStrictReindex: without junk the plain
// command is the right one, and its wording does not change.
func TestAdviceOnCleanLiveRecordingKeepsTheStrictReindex(t *testing.T) {
	d, err := Diagnose(context.Background(), liveFile(t, livefixture.Options{}))
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	for _, f := range d.Findings {
		if f.Kind == "damaged" || strings.Contains(f.Remedy, "--resync") {
			t.Errorf("clean live recording: [%s] %q / %q", f.Kind, f.Detail, f.Remedy)
		}
	}
}

// TestEditMetadataBehindHeadJunk: a metadata edit copies the clusters with a
// strict walk, which stopped on the junk the reader had just resynced past.
// With nothing written yet it now falls back to the block rewrite, as it does
// for a live source.
func TestEditMetadataBehindHeadJunk(t *testing.T) {
	for name, o := range map[string]livefixture.Options{
		"junk after the Segment header": {JunkHead: 134, ShortUnknown: true},
		"junk between Info and Tracks":  {JunkMid: 134, SizedSegment: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dst := filepath.Join(t.TempDir(), "edited.mkv")
			err := EditMetadata(ctx, liveFile(t, o), dst, func(c *mkv.Container) { c.Info.Title = "edited" })
			if err != nil {
				t.Fatalf("EditMetadata: %v", err)
			}
			checkLiveOutput(t, dst)
			c, err := reader.Open(ctx, dst)
			if err != nil {
				t.Fatalf("open output: %v", err)
			}
			if c.Info.Title != "edited" || c.ResyncedBytes != 0 {
				t.Errorf("title = %q, resynced bytes = %d; want the edit applied and a clean file", c.Info.Title, c.ResyncedBytes)
			}
		})
	}
}

// TestRewriteKeepsLacedAudio: a block-by-block rewrite of a live source keeps
// each audio lace as a lace. Delaced, the frames of one block - which share
// its timecode when the track states no frame duration - became separate
// blocks all claiming the same instant.
func TestRewriteKeepsLacedAudio(t *testing.T) {
	ctx := context.Background()
	src := liveFile(t, livefixture.Options{LacedAudio: true})
	dst := filepath.Join(t.TempDir(), "edited.mkv")
	if err := EditMetadata(ctx, src, dst, func(c *mkv.Container) { c.Info.Title = "edited" }); err != nil {
		t.Fatalf("EditMetadata: %v", err)
	}
	type frame struct {
		track    uint64
		timecode int64
		laced    bool
		size     int
	}
	frames := func(path string) []frame {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		br, err := reader.NewBlockReader(f, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		var out []frame
		for {
			b, err := br.Next()
			if errors.Is(err, io.EOF) {
				return out
			}
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			out = append(out, frame{b.TrackNumber, b.Timecode, b.Laced, len(b.Data)})
		}
	}
	want, got := frames(src), frames(dst)
	if len(want) != len(got) {
		t.Fatalf("frames: %d in the source, %d after the rewrite", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("frame %d: %+v in the source, %+v after the rewrite", i, want[i], got[i])
		}
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
