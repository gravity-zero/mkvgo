package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
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

// TestResyncRollbackDeltaStaysSmall: a cluster whose position hints are
// restated on the way out differs from the source by those few bytes. The
// tolerant walk used to put the WHOLE body of every such cluster in the
// rollback delta as a literal - 19.9 MB of delta for a 20 MB file. Only the
// restated spans are literals; the delta still rebuilds the source exactly.
func TestResyncRollbackDeltaStaysSmall(t *testing.T) {
	ctx := context.Background()
	o := livefixture.Options{JunkHead: 134, PositionHints: true, TailTags: true}
	src := liveFile(t, o)
	data := livefixture.Build(o)
	dir := t.TempDir()
	dst := filepath.Join(dir, "resync.mkv")
	var delta bytes.Buffer
	if err := Reindex(ctx, src, dst, mkv.Options{Resync: true, RollbackSink: &delta}); err != nil {
		t.Fatalf("Reindex with Resync: %v", err)
	}
	checkLiveOutput(t, dst)

	// The run of blocks closing the first cluster is media the output holds
	// verbatim: it must be a COPY in the delta, not bytes carried in it.
	second := bytes.Index(data[bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})+4:], []byte{0x1F, 0x43, 0xB6, 0x75})
	first := bytes.Index(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	media := data[first+4+second-40 : first+4+second]
	if bytes.Contains(delta.Bytes(), media) {
		t.Error("the rollback delta carries cluster media the output already holds")
	}

	restored := filepath.Join(dir, "restored.mkv")
	if err := ApplyRollback(ctx, dst, &delta, restored); err != nil {
		t.Fatalf("ApplyRollback: %v", err)
	}
	if got, _ := os.ReadFile(restored); !bytes.Equal(got, data) {
		t.Error("rollback did not rebuild the source byte for byte")
	}
}

// TestRefusedReindexLeavesNoOutput: a copy that is refused part-way used to
// leave what it had written - a truncated file under the output's name, which
// a later step can take for the result. It is removed. A file the operation
// never created is not touched: a source that cannot even be opened leaves a
// pre-existing output as it was.
func TestRefusedReindexLeavesNoOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mkv")

	src := liveFile(t, livefixture.Options{JunkHead: 134})
	if err := Reindex(ctx, src, dst); err == nil {
		t.Fatal("strict Reindex accepted the junk")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("the refused reindex left an output behind (stat: %v)", err)
	}

	writeAll(t, dst, []byte("somebody else's file"))
	if err := Reindex(ctx, filepath.Join(dir, "missing.mkv"), dst); err == nil {
		t.Fatal("Reindex of a missing source succeeded")
	}
	if got, _ := os.ReadFile(dst); string(got) != "somebody else's file" {
		t.Errorf("a reindex that never created its output altered the file already there: %q", got)
	}
}

// TestReindexSealsTheDurationOfALiveRecording: a live source declares no
// Duration - its muxer never came back to write one. The rewrite that seals
// its sizes left it that way: sized, indexed, and still of unknown length. It
// now states where the content ends, in an Info that is otherwise the source's
// (a CRC-32 opening it is resealed), and the rollback delta still rebuilds the
// source byte for byte. A source that declares a Duration keeps its Info
// untouched.
func TestReindexSealsTheDurationOfALiveRecording(t *testing.T) {
	ctx := context.Background()
	// The latest end: the audio's last block plus its stride (250 ms).
	const wantMs = livefixture.LastBlockMs + 250
	for name, tc := range map[string]struct {
		opts   livefixture.Options
		resync bool
	}{
		"strict":                 {livefixture.Options{}, false},
		"strict, Info with CRC":  {livefixture.Options{InfoCRC: true, TailTags: true}, false},
		"resync past head junk":  {livefixture.Options{JunkHead: 134, InfoCRC: true}, true},
		"resync, no CRC, hints":  {livefixture.Options{JunkMid: 61, PositionHints: true}, true},
		"strict, sized Segment":  {livefixture.Options{SizedSegment: true, InfoCRC: true}, false},
		"strict, laced audio":    {livefixture.Options{LacedAudio: true, BlockGroups: true}, false},
		"salvage past head junk": {livefixture.Options{JunkHead: 134, InfoCRC: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			src := liveFile(t, tc.opts)
			dir := t.TempDir()
			dst := filepath.Join(dir, "out.mkv")
			var delta bytes.Buffer
			var err error
			if strings.HasPrefix(name, "salvage") {
				_, err = Salvage(ctx, src, dst, mkv.Options{RollbackSink: &delta})
			} else {
				err = Reindex(ctx, src, dst, mkv.Options{Resync: tc.resync, RollbackSink: &delta})
			}
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			c, err := reader.Open(ctx, dst)
			if err != nil {
				t.Fatalf("open output: %v", err)
			}
			if c.DurationMs != wantMs {
				t.Errorf("output declares %d ms, want %d (where the content ends)", c.DurationMs, wantMs)
			}
			issues, err := Validate(ctx, dst)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			for _, is := range issues {
				if is.Code == "no-duration" {
					t.Errorf("output still reports: %s", is.Message)
				}
			}
			if tc.opts.InfoCRC && !infoCRCHolds(t, dst) {
				t.Error("the Info CRC-32 no longer matches its body")
			}
			restored := filepath.Join(dir, "restored.mkv")
			if err := ApplyRollback(ctx, dst, &delta, restored); err != nil {
				t.Fatalf("ApplyRollback: %v", err)
			}
			if got, _ := os.ReadFile(restored); !bytes.Equal(got, livefixture.Build(tc.opts)) {
				t.Error("rollback did not rebuild the source byte for byte")
			}
		})
	}

	// A source that declares its duration: the Info is copied as it is.
	dst := filepath.Join(t.TempDir(), "sample.out.mkv")
	if err := Reindex(ctx, sampleMKV, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if a, b := infoBody(t, sampleMKV), infoBody(t, dst); !bytes.Equal(a, b) {
		t.Error("the Info of a source that declares a Duration was not copied verbatim")
	}
}

// infoBody returns the body of path's Info element. The Info ID also appears
// in the SeekHead, as a SeekID value followed by a SeekPosition: that
// occurrence is skipped.
func infoBody(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id := []byte{0x15, 0x49, 0xA9, 0x66}
	for off := bytes.Index(data, id); off >= 0; {
		if !bytes.HasPrefix(data[off+4:], []byte{0x53, 0xAC}) {
			h, n, err := ebml.ReadElementHeader(bytes.NewReader(data[off:]))
			if err == nil && h.Size > 0 && off+n+int(h.Size) <= len(data) {
				return data[off+n : off+n+int(h.Size)]
			}
		}
		next := bytes.Index(data[off+1:], id)
		if next < 0 {
			break
		}
		off += 1 + next
	}
	t.Fatalf("%s: no Info element found", path)
	return nil
}

// infoCRCHolds reports whether the CRC-32 opening path's Info matches the
// rest of its body.
func infoCRCHolds(t *testing.T, path string) bool {
	t.Helper()
	body := infoBody(t, path)
	if len(body) < 6 || body[0] != 0xBF || body[1] != 0x84 {
		t.Fatalf("%s: the Info does not open with a CRC-32", path)
	}
	return binary.LittleEndian.Uint32(body[2:6]) == crc32.ChecksumIEEE(body[6:])
}

// TestZeroedHoleLongerThanTheScanCap: a zeroed region - the pieces a download
// never received - is walked to its end whatever its length. The scan cap used
// to count it: a file missing more than the cap in its middle was refused
// outright ("no valid cluster found"), sound media behind the hole included.
// The cap still bounds a search through actual garbage of that length.
func TestZeroedHoleLongerThanTheScanCap(t *testing.T) {
	saved := salvageResyncCap
	salvageResyncCap = 256
	defer func() { salvageResyncCap = saved }()

	ctx := context.Background()
	data := livefixture.Build(livefixture.Options{})
	cluster := []byte{0x1F, 0x43, 0xB6, 0x75}
	second := bytes.Index(data[bytes.Index(data, cluster)+4:], cluster) + bytes.Index(data, cluster) + 4
	holed := func(fill byte) string {
		out := append([]byte(nil), data[:second]...)
		out = append(out, bytes.Repeat([]byte{fill}, 4096)...) // 16x the cap
		out = append(out, data[second:]...)
		path := filepath.Join(t.TempDir(), "holed.mkv")
		writeAll(t, path, out)
		return path
	}

	report, err := MapDamage(ctx, holed(0x00))
	if err != nil {
		t.Fatalf("MapDamage over a zeroed hole: %v", err)
	}
	if report.BytesSkipped != 4096 || report.ClustersCopied != livefixture.Clusters {
		t.Errorf("skipped %d bytes, copied %d clusters; want the 4096 zeroed bytes and all %d clusters",
			report.BytesSkipped, report.ClustersCopied, livefixture.Clusters)
	}
	if _, err := MapDamage(ctx, holed(livefixture.JunkByte)); err == nil {
		t.Error("garbage longer than the scan cap was searched through to the end")
	}
}

// countingFS counts the bytes read through it.
type countingFile struct {
	mkv.ReadSeekCloser
	n *int64
}

func (c countingFile) Read(p []byte) (int, error) {
	n, err := c.ReadSeekCloser.Read(p)
	*c.n += int64(n)
	return n, err
}

// TestSurgicalCandidateDoesNotReadItsPayload: every byte of a damaged region
// that looks like the start of a block is tested as a resume point, and the
// test used to read the whole "payload" the candidate declared before looking
// at its track number. On a real 2.7 GiB file with missing pieces that read
// about a hundred GiB (eight minutes) to map the damage. A candidate naming an
// undeclared track, or a size no block has, is now refused on its header.
func TestSurgicalCandidateDoesNotReadItsPayload(t *testing.T) {
	// One valid block header for track 1 claiming 32 MiB of payload, then
	// zeros: chain-walked from its first byte, it must be judged on what
	// follows without that payload being read through.
	const claimed = 32 << 20
	candidate := []byte{0xA3, 0x12, 0x00, 0x00, 0x00, 0x99, 0x00, 0x00, 0x80} // track 0x99: undeclared
	candidate[1], candidate[2], candidate[3], candidate[4] = 0x12, 0x00, 0x00, 0x00
	binary.BigEndian.PutUint32(candidate[1:5], 0x10000000|claimed)
	data := append(candidate, make([]byte, claimed+1024)...)
	path := filepath.Join(t.TempDir(), "candidate.bin")
	writeAll(t, path, data)

	var read int64
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	run, stop, err := chainWalkChildren(countingFile{f, &read}, 0, int64(len(data)), map[uint64]bool{1: true, 2: true})
	if err != nil {
		t.Fatal(err)
	}
	if stop != surgicalStopBreak || run.children != 0 {
		t.Fatalf("a block of an undeclared track was accepted: stop %v, %d children", stop, run.children)
	}
	if read > 1<<20 {
		t.Errorf("read %d bytes to refuse a candidate on its header", read)
	}
}

// TestRecoveryDoesNotAttachALaterClusterToTheBrokenOne: when a gap swallows a
// Cluster header, the blocks behind it belong to that lost cluster - their
// relative timecodes count from ITS timestamp. Their timecodes restart near
// where the broken cluster stopped, so the continuity gate let them through
// as its continuation, timed against the wrong base: on a real download with
// missing pieces the audio stepped back half a second after every hole. A
// resume must also account for the bytes it skipped: far too little time for
// a gap that long means another cluster.
func TestRecoveryDoesNotAttachALaterClusterToTheBrokenOne(t *testing.T) {
	ctx := context.Background()
	data := livefixture.Build(livefixture.Options{})
	cluster := []byte{0x1F, 0x43, 0xB6, 0x75}
	first := bytes.Index(data, cluster)
	second := first + 4 + bytes.Index(data[first+4:], cluster)
	// The second cluster loses its header and Timestamp (12 + 4 bytes) to
	// 4096 bytes of junk; its blocks follow, headerless.
	holed := append([]byte(nil), data[:second]...)
	holed = append(holed, bytes.Repeat([]byte{livefixture.JunkByte}, 4096)...)
	holed = append(holed, data[second+16:]...)
	src := filepath.Join(t.TempDir(), "holed.mkv")
	writeAll(t, src, holed)

	dst := filepath.Join(t.TempDir(), "out.mkv")
	if _, err := Salvage(ctx, src, dst); err != nil {
		t.Fatalf("Salvage: %v", err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	last := map[uint64]int64{}
	n := 0
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if prev, ok := last[b.TrackNumber]; ok && b.Timecode <= prev {
			t.Fatalf("track %d steps back from %d ms to %d ms: a later cluster's blocks were timed against the broken one", b.TrackNumber, prev, b.Timecode)
		}
		last[b.TrackNumber] = b.Timecode
		n++
	}
	// The first and the third cluster, whole; the headerless blocks of the
	// second cannot be timed and are left out.
	if want := 2 * livefixture.BlocksPerTrack * livefixture.Tracks; n != want {
		t.Errorf("%d blocks recovered, want %d (the two clusters whose timestamps are known)", n, want)
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

// TestBlockStartPlausible: the in-memory judgement a candidate block start
// gets before the file is touched for it. On a real damaged file almost every
// candidate is a payload byte that happens to equal a block ID; walking each
// one from disk read the file eight times over.
func TestBlockStartPlausible(t *testing.T) {
	tracks := map[uint64]bool{1: true, 2: true}
	pad := make([]byte, 16)
	for name, tc := range map[string]struct {
		b                  []byte
		room               int64
		plausible, decided bool
	}{
		"SimpleBlock of a declared track":   {append([]byte{0xA3, 0x87, 0x81, 0, 0, 0x80}, pad...), 1 << 20, true, true},
		"SimpleBlock of an undeclared one":  {append([]byte{0xA3, 0x87, 0x99, 0, 0, 0x80}, pad...), 1 << 20, false, true},
		"SimpleBlock larger than any block": {append([]byte{0xA3, 0x08, 0x40, 0x00, 0x01, 0x81}, pad...), 1 << 40, false, true},
		"SimpleBlock running past the file": {append([]byte{0xA3, 0x87, 0x81, 0, 0, 0x80}, pad...), 4, false, true},
		"BlockGroup opening on its Block":   {append([]byte{0xA0, 0x8A, 0xA1, 0x86, 0x82, 0, 0, 0}, pad...), 1 << 20, true, true},
		"BlockGroup, undeclared track":      {append([]byte{0xA0, 0x8A, 0xA1, 0x86, 0x99, 0, 0, 0}, pad...), 1 << 20, false, true},
		"BlockGroup opening on garbage":     {append([]byte{0xA0, 0x8A, 0x77, 0x86, 0x82, 0, 0, 0}, pad...), 1 << 20, false, true},
		"BlockGroup opening on a duration":  {append([]byte{0xA0, 0x8A, 0x9B, 0x81, 0x28, 0xA1, 0x84, 0x81}, pad...), 1 << 20, true, true},
		"too few bytes to tell":             {[]byte{0xA3}, 1 << 20, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			plausible, decided := blockStartPlausible(tc.b, tc.room, tracks)
			if plausible != tc.plausible || decided != tc.decided {
				t.Errorf("plausible=%v decided=%v, want %v %v", plausible, decided, tc.plausible, tc.decided)
			}
		})
	}
}

// TestTolerantWalkReadsAHoledFileOnce: mapping the damage of a file with a
// lost region is one pass over it. It read a real 2.7 GiB download with
// missing pieces eight to forty times over (21 to 100 GiB, up to 499 s); the
// candidates of a resume point are now judged in memory, and a run the time
// gates refuse is refused whole.
func TestTolerantWalkReadsAHoledFileOnce(t *testing.T) {
	// Clusters with the weight of real ones (4 KiB blocks), so a re-read of
	// the damaged region shows: 30 of them, then 10 whose header and
	// Timestamp were lost to junk - their blocks remain, headerless - then
	// 30 more.
	one := livefixture.Build(livefixture.Options{PayloadBytes: 4096})
	cluster := []byte{0x1F, 0x43, 0xB6, 0x75}
	head := one[:bytes.Index(one, cluster)]
	body := one[bytes.Index(one, cluster):]
	lost := append([]byte(nil), body...)
	for j := 0; j+16 <= len(lost); j++ {
		if bytes.Equal(lost[j:j+4], cluster) {
			copy(lost[j:j+16], bytes.Repeat([]byte{livefixture.JunkByte}, 16))
		}
	}
	data := append([]byte(nil), head...)
	for i := 0; i < 10; i++ {
		data = append(data, body...)
	}
	holeAt := len(data)
	for i := 0; i < 4; i++ {
		data = append(data, lost...)
	}
	holeEnd := len(data)
	for i := 0; i < 10; i++ {
		data = append(data, body...)
	}
	path := filepath.Join(t.TempDir(), "holed.mkv")
	writeAll(t, path, data)

	var read int64
	fs := &mkv.FS{
		Open: func(p string) (mkv.ReadSeekCloser, error) {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			return countingFile{f, &read}, nil
		},
	}
	report, err := MapDamage(context.Background(), path, mkv.Options{FS: fs})
	if err != nil {
		t.Fatalf("MapDamage: %v", err)
	}
	if report.BytesSkipped < int64(holeEnd-holeAt)/2 {
		t.Errorf("skipped %d bytes, want most of the %d-byte region of headerless clusters", report.BytesSkipped, holeEnd-holeAt)
	}
	// The head-only reads and the walk itself: a small multiple of the file,
	// never the tens of passes a per-candidate file read costs.
	if limit := int64(len(data)) * 4; read > limit {
		t.Errorf("read %d bytes to map a %d-byte file (more than %d): the damaged region is being re-read", read, len(data), limit)
	}
}

// TestBlockCutByAZeroedHoleIsDropped: a zeroed hole (pieces a download never
// received) usually begins in the middle of a block. That block's header is
// intact, so the chain accepted it - with a payload whose tail is the hole's
// zeros. Kept, it is one frame a decoder chokes on at the leading edge of every
// hole (measured on a real file: 9 holes, 9 decode errors, present in the
// source and carried into the repair). It is dropped with the hole.
func TestBlockCutByAZeroedHoleIsDropped(t *testing.T) {
	data := livefixture.Build(livefixture.Options{PayloadBytes: 4096})
	cluster := []byte{0x1F, 0x43, 0xB6, 0x75}
	first := bytes.Index(data, cluster)
	second := first + 4 + bytes.Index(data[first+4:], cluster)
	third := second + 4 + bytes.Index(data[second+4:], cluster)
	// The hole starts 1 KiB into the last block of the first cluster and
	// swallows the whole second cluster.
	lastBlock := bytes.LastIndex(data[:second], []byte{0xA3})
	for lastBlock > first && data[lastBlock+3] != 0x82 { // the SimpleBlock of track 2, not a payload byte
		lastBlock = bytes.LastIndex(data[:lastBlock], []byte{0xA3})
	}
	holed := append([]byte(nil), data...)
	for i := lastBlock + 1024; i < third; i++ {
		holed[i] = 0
	}
	src := filepath.Join(t.TempDir(), "holed.mkv")
	writeAll(t, src, holed)
	dst := filepath.Join(t.TempDir(), "out.mkv")
	if _, err := Salvage(context.Background(), src, dst); err != nil {
		t.Fatalf("Salvage: %v", err)
	}

	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		b, err := br.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.HasSuffix(b.Data, make([]byte, 64)) {
			t.Fatalf("block %d (track %d, %d ms) was kept with a zeroed tail: the hole began inside it", n, b.TrackNumber, b.Timecode)
		}
		n++
	}
	// The first cluster less its cut block, and the third cluster whole.
	if want := 2*livefixture.BlocksPerTrack*livefixture.Tracks - 1; n != want {
		t.Errorf("%d blocks recovered, want %d", n, want)
	}
}
