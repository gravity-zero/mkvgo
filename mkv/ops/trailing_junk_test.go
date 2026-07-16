package ops

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// trailing_junk_test.go - the trailing-junk contract. A few surplus bytes
// past the declared Segment end (zero padding is the wild signature: batch
// tools pad their output, every reader stops at the declared end) must not
// make a file unrepairable: the rewrites drop them and report the range,
// while REAL media past an undershooting declared size keeps the strict
// refusal, and the tolerant walk stops calling the surplus a truncation.

// junkedFixture builds a clean multi-cluster file and a sibling with junk
// appended past the declared Segment end, returning both paths and the
// clean length (= the declared end = where the junk starts).
func junkedFixture(t *testing.T, dir string, junk []byte) (clean, junked string, cleanLen int64) {
	t.Helper()
	clean = salvageFixture(t, dir, "clean.mkv", 6)
	data := readAll(t, clean)
	junked = filepath.Join(dir, "junked.mkv")
	writeAll(t, junked, append(append([]byte{}, data...), junk...))
	return clean, junked, int64(len(data))
}

// makeSegmentStreamed rewrites data's Segment size as the unknown-size VINT
// of the same length (all value bits set), turning a sealed file into a
// streamed one.
func makeSegmentStreamed(t *testing.T, data []byte) []byte {
	t.Helper()
	segOff := findAll(data, []byte{0x18, 0x53, 0x80, 0x67})[0]
	h, n, err := ebml.ReadElementHeader(bytes.NewReader(data[segOff:]))
	if err != nil || h.ID != mkv.IDSegment {
		t.Fatalf("segment header at %d does not parse: %v", segOff, err)
	}
	sizeLen := int64(n - 4) // element ID is the 4 bytes matched above
	out := append([]byte{}, data...)
	out[segOff+4] = byte(0xFF >> (sizeLen - 1))
	for i := int64(1); i < sizeLen; i++ {
		out[segOff+4+i] = 0xFF
	}
	return out
}

// TestReindex_DropsTrailingJunk: the strict rewrite must not abort on the
// zero bytes past the declared Segment end; it drops them, reports the exact
// range through OnSkip, and produces the same output the clean source
// produces. DeepVerify stays meaningful: the junk lies where the block walk
// never reads.
func TestReindex_DropsTrailingJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clean, junked, cleanLen := junkedFixture(t, dir, make([]byte, 8))

	outClean := filepath.Join(dir, "out_clean.mkv")
	if err := Reindex(ctx, clean, outClean); err != nil {
		t.Fatalf("Reindex clean: %v", err)
	}

	var skipped []DamagedRange
	outJunk := filepath.Join(dir, "out_junk.mkv")
	err := Reindex(ctx, junked, outJunk, mkv.Options{
		DeepVerify: true,
		OnSkip:     func(r DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex must drop trailing junk, got: %v", err)
	}
	if len(skipped) != 1 || skipped[0].StartOffset != cleanLen || skipped[0].EndOffset != cleanLen+8 {
		t.Errorf("OnSkip = %+v, want exactly [%d, %d)", skipped, cleanLen, cleanLen+8)
	}
	if !bytes.Equal(readAll(t, outClean), readAll(t, outJunk)) {
		t.Error("the junked source must rewrite byte-identical to the clean source")
	}
}

// TestReindex_RealClusterPastDeclaredEndKept: a structurally valid Cluster
// past the declared Segment end is real media behind an undershooting size,
// not junk - the walk must keep copying it, and nothing may be reported
// dropped.
func TestReindex_RealClusterPastDeclaredEndKept(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clean := salvageFixture(t, dir, "clean.mkv", 6)
	data := readAll(t, clean)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if len(offsets) < 2 {
		t.Fatalf("fixture has only %d clusters", len(offsets))
	}
	// The last cluster's bytes, appended verbatim past the declared end.
	extra := filepath.Join(dir, "extra.mkv")
	writeAll(t, extra, append(append([]byte{}, data...), data[offsets[len(offsets)-1]:]...))

	var skipped []DamagedRange
	out := filepath.Join(dir, "out.mkv")
	err := Reindex(ctx, extra, out, mkv.Options{
		OnSkip: func(r DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("real media past the declared end was reported dropped: %+v", skipped)
	}
	if n := blockReaderIteratesCleanly(t, out); n == 0 {
		t.Error("output does not decode")
	}
	// The duplicated final cluster must be IN the output: the junked-length
	// output must be strictly longer than the plain rewrite.
	outClean := filepath.Join(dir, "out_clean.mkv")
	if err := Reindex(ctx, clean, outClean); err != nil {
		t.Fatal(err)
	}
	if len(readAll(t, out)) <= len(readAll(t, outClean)) {
		t.Error("the trailing real cluster was silently dropped")
	}
}

// TestReindex_JunkHidingAClusterStaysStrict: unparseable bytes past the
// declared end FOLLOWED by a valid Cluster are not provable junk - dropping
// to EOF would drop the cluster with them. The strict refusal must hold,
// typed ErrCorruptSource, so the caller routes to Resync deliberately.
func TestReindex_JunkHidingAClusterStaysStrict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clean := salvageFixture(t, dir, "clean.mkv", 6)
	data := readAll(t, clean)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	tail := append(make([]byte, 8), data[offsets[len(offsets)-1]:]...)
	mixed := filepath.Join(dir, "mixed.mkv")
	writeAll(t, mixed, append(append([]byte{}, data...), tail...))

	err := Reindex(ctx, mixed, filepath.Join(dir, "out.mkv"))
	if err == nil {
		t.Fatal("junk hiding a valid cluster must keep the strict refusal")
	}
	if !errors.Is(err, ErrCorruptSource) {
		t.Errorf("the strict refusal must wrap ErrCorruptSource, got: %v", err)
	}
}

// TestReindex_JunkMasqueradingAsElementDropped: junk that happens to parse
// as an element header whose size overruns the end of the file cannot be a
// real element - it takes the same drop path as unparseable junk.
func TestReindex_JunkMasqueradingAsElementDropped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// 0x4286 (EBMLVersion) claiming a 256-byte body, with 4 bytes present.
	masq := []byte{0x42, 0x86, 0x41, 0x00, 0xDE, 0xAD, 0xBE, 0xEF}
	clean, junked, cleanLen := junkedFixture(t, dir, masq)

	var skipped []DamagedRange
	out := filepath.Join(dir, "out.mkv")
	err := Reindex(ctx, junked, out, mkv.Options{
		OnSkip: func(r DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex must drop the masquerading junk, got: %v", err)
	}
	if len(skipped) != 1 || skipped[0].StartOffset != cleanLen || skipped[0].EndOffset != cleanLen+int64(len(masq)) {
		t.Errorf("OnSkip = %+v, want exactly [%d, %d)", skipped, cleanLen, cleanLen+int64(len(masq)))
	}
	outClean := filepath.Join(dir, "out_clean.mkv")
	if err := Reindex(ctx, clean, outClean); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, outClean), readAll(t, out)) {
		t.Error("the junked source must rewrite byte-identical to the clean source")
	}
}

// TestReindex_RollbackDeltaCoversDroppedJunk: the inverse delta must keep
// reconstructing the ORIGINAL - junk included - even though the output has
// no trace of the dropped bytes.
func TestReindex_RollbackDeltaCoversDroppedJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, junked, _ := junkedFixture(t, dir, []byte{0x00, 0x00, 0x00, 0x00})
	original := readAll(t, junked)

	var delta bytes.Buffer
	out := filepath.Join(dir, "out.mkv")
	if err := Reindex(ctx, junked, out, mkv.Options{RollbackSink: &delta}); err != nil {
		t.Fatalf("Reindex with RollbackSink: %v", err)
	}
	rollbackRoundTrip(t, out, delta.Bytes(), original)
}

// TestRetimeTracksReplace_ShiftsDespiteTrailingJunk is the wild signature
// end to end: a multi-track file with a late audio track AND a few zero
// bytes past the declared Segment end. The rewrite must shift the track,
// drop the junk, and leave a file Diagnose calls healthy.
func TestRetimeTracksReplace_ShiftsDespiteTrailingJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}},
		})
	}
	late := buildMultiClusterMKV(t, dir, "late.mkv", tracks, sets, 4000)
	data := readAll(t, late)
	junked := filepath.Join(dir, "late_junked.mkv")
	writeAll(t, junked, append(append([]byte{}, data...), make([]byte, 8)...))

	var skipped []DamagedRange
	err := RetimeTracksReplace(ctx, junked, map[uint64]int64{2: -300_000_000}, mkv.Options{
		DeepVerify: true,
		OnSkip:     func(r DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("RetimeTracksReplace must repair despite trailing junk, got: %v", err)
	}
	if len(skipped) != 1 || skipped[0].StartOffset != int64(len(data)) {
		t.Errorf("OnSkip = %+v, want the junk range from %d", skipped, len(data))
	}

	d, err := Diagnose(ctx, junked)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Healthy {
		t.Errorf("the repaired file must diagnose healthy, got findings %v", findingKinds(d))
	}
	if d.AudioDelaysNs[2] != 0 {
		t.Errorf("audio delay after retime = %dns, want 0", d.AudioDelaysNs[2])
	}
}

// TestRetimeTracksInPlace_NotBlockedByJunk: the in-place engine's scan is
// bounded by the declared Segment end, so the junk is never even read (an
// explicit engine choice - the automatic mode routes this small fixture to
// the rewrite). The shift lands; the junk stays (in place cannot drop bytes)
// and keeps being reported by Diagnose as trailing junk, NOT as a truncated
// download.
func TestRetimeTracksInPlace_NotBlockedByJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}},
		})
	}
	late := buildMultiClusterMKV(t, dir, "late.mkv", tracks, sets, 4000)
	data := readAll(t, late)
	junked := filepath.Join(dir, "late_junked.mkv")
	writeAll(t, junked, append(append([]byte{}, data...), make([]byte, 4)...))

	if err := RetimeTracksInPlace(ctx, junked, map[uint64]int64{2: -300_000_000}); err != nil {
		t.Fatalf("RetimeTracksInPlace on a junked file: %v", err)
	}
	after := readAll(t, junked)
	if int64(len(after)) != int64(len(data))+4 {
		t.Errorf("in place must leave the junk in the file: %d bytes, want %d", len(after), len(data)+4)
	}
	d, err := Diagnose(ctx, junked)
	if err != nil {
		t.Fatal(err)
	}
	if d.AudioDelaysNs[2] != 0 {
		t.Errorf("audio delay after retime = %dns, want 0", d.AudioDelaysNs[2])
	}
	if hasFinding(d, "trailing-junk") == nil {
		t.Errorf("the junk is still there and must keep being reported: %v", findingKinds(d))
	}
	if hasFinding(d, "truncated") != nil {
		t.Errorf("surplus bytes must never diagnose truncated: %v", findingKinds(d))
	}
	if d.Damage == nil || d.Damage.TruncatedTail {
		t.Error("surplus bytes must never set TruncatedTail")
	}
}

// TestMapDamage_TrailingJunkIsNotTruncated: the tolerant walk still counts
// the surplus bytes as a damaged range (they ARE dropped by a repair) but
// must not raise the truncated-tail verdict - bytes in excess are not bytes
// missing, and the verdict's remedy is "re-download".
func TestMapDamage_TrailingJunkIsNotTruncated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, junked, cleanLen := junkedFixture(t, dir, make([]byte, 8))

	report, err := MapDamage(ctx, junked)
	if err != nil {
		t.Fatalf("MapDamage: %v", err)
	}
	if report.TruncatedTail {
		t.Error("TruncatedTail = true for surplus bytes; truncated means bytes MISSING")
	}
	if report.BytesSkipped != 8 {
		t.Errorf("BytesSkipped = %d, want 8", report.BytesSkipped)
	}
	if len(report.DamagedRanges) != 1 || report.DamagedRanges[0].StartOffset != cleanLen {
		t.Errorf("DamagedRanges = %+v, want one range from %d", report.DamagedRanges, cleanLen)
	}
}

// TestMapDamage_MidSegmentDamageToEOFStillTruncated pins the other side of
// the TruncatedTail boundary: damage that BEGINS inside the declared Segment
// and runs to EOF keeps the truncated verdict, junk beyond the declared end
// or not - real media is missing, whatever else the tail carries.
func TestMapDamage_MidSegmentDamageToEOFStillTruncated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, junked, _ := junkedFixture(t, dir, make([]byte, 8))
	data := readAll(t, junked)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	if len(offsets) < 5 {
		t.Fatalf("fixture has only %d clusters", len(offsets))
	}
	// Zero everything from the 5th cluster through EOF: structural damage
	// starting well inside the declared Segment, swallowing the junk too.
	for i := offsets[4]; i < int64(len(data)); i++ {
		data[i] = 0x00
	}
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data)

	report, err := MapDamage(ctx, cut)
	if err != nil {
		t.Fatalf("MapDamage: %v", err)
	}
	if !report.TruncatedTail {
		t.Error("EOF-reaching damage starting inside the declared Segment must keep TruncatedTail")
	}
}

// TestReindex_TruncatedClusterPastDeclaredEndStaysStrict is the regression
// the adversarial review caught: a REAL cluster appended past an
// undershooting declared Segment size and then CUT mid-write must not pass
// the junk proof - structural validation cannot see a cut cluster, but its
// 4-byte ID is right there, and dropping it would silently destroy media the
// strict walk exists to protect.
func TestReindex_TruncatedClusterPastDeclaredEndStaysStrict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clean := salvageFixture(t, dir, "clean.mkv", 6)
	data := readAll(t, clean)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	// The last cluster's exact extent, from its own header (the bytes after
	// it belong to the trailing index, which is legitimately droppable).
	off := offsets[len(offsets)-1]
	h, n, err := ebml.ReadElementHeader(bytes.NewReader(data[off:]))
	if err != nil || h.ID != mkv.IDCluster {
		t.Fatalf("cluster header at %d does not parse: %v", off, err)
	}
	last := data[off : off+int64(n)+h.Size]
	cut := last[:len(last)-10] // a real cluster, torn 10 bytes early

	for name, tail := range map[string][]byte{
		"cut cluster alone":      cut,
		"zeros then cut cluster": append(make([]byte, 8), cut...),
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".mkv")
		writeAll(t, path, append(append([]byte{}, data...), tail...))
		rerr := Reindex(ctx, path, path+".out")
		if rerr == nil {
			t.Fatalf("%s: a torn real cluster past the declared end was dropped as junk", name)
		}
		if !errors.Is(rerr, ErrCorruptSource) {
			t.Errorf("%s: the strict refusal must wrap ErrCorruptSource, got: %v", name, rerr)
		}
	}
}

// TestReindex_TornHeaderJunkAtEOF covers the junk shape that reads as a
// clean io.EOF: a complete element ID with its size VINT missing (what a
// torn write leaves). It must take the same drop path as any junk - reported,
// and carried by the rollback delta - not vanish silently; and when those
// bytes are a bare Cluster ID, they are a trace of real media and must keep
// the strict refusal instead.
func TestReindex_TornHeaderJunkAtEOF(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A lone Void ID: provable junk, dropped and reported.
	cleanPath, junked, cleanLen := junkedFixture(t, dir, []byte{0xEC})
	original := readAll(t, junked)
	var skipped []DamagedRange
	var delta bytes.Buffer
	out := filepath.Join(dir, "out.mkv")
	err := Reindex(ctx, junked, out, mkv.Options{
		RollbackSink: &delta,
		OnSkip:       func(r DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex on torn-header junk: %v", err)
	}
	if len(skipped) != 1 || skipped[0].StartOffset != cleanLen || skipped[0].EndOffset != cleanLen+1 {
		t.Errorf("OnSkip = %+v, want exactly [%d, %d)", skipped, cleanLen, cleanLen+1)
	}
	rollbackRoundTrip(t, out, delta.Bytes(), original)

	// A bare Cluster ID at EOF: the trace of a cut cluster, never junk.
	magicOnly := filepath.Join(dir, "magic_only.mkv")
	clean := readAll(t, cleanPath)
	writeAll(t, magicOnly, append(append([]byte{}, clean...), 0x1F, 0x43, 0xB6, 0x75))
	err = Reindex(ctx, magicOnly, filepath.Join(dir, "magic_out.mkv"))
	if err == nil {
		t.Fatal("a bare Cluster ID at EOF was dropped as junk")
	}
	if !errors.Is(err, ErrCorruptSource) {
		t.Errorf("the strict refusal must wrap ErrCorruptSource, got: %v", err)
	}
}

// TestReindex_JunkLongerThanScanWindowStaysStrict: junk cannot be proven
// junk past the bounded scan window, so it keeps the strict refusal instead
// of an unbounded read (or an unproven drop).
func TestReindex_JunkLongerThanScanWindowStaysStrict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	saved := salvageResyncCap
	salvageResyncCap = 1024
	defer func() { salvageResyncCap = saved }()

	_, junked, _ := junkedFixture(t, dir, make([]byte, 4096))
	err := Reindex(ctx, junked, filepath.Join(dir, "out.mkv"))
	if err == nil {
		t.Fatal("junk longer than the scan window must keep the strict refusal")
	}
	if !errors.Is(err, ErrCorruptSource) {
		t.Errorf("the strict refusal must wrap ErrCorruptSource, got: %v", err)
	}
}

// TestReindexResync_DropsTrailingJunk: the tolerant engine keeps its own
// junk handling - same outcome, same OnSkip report - so a caller escalating
// from strict to Resync sees no surprise on a junk-only file.
func TestReindexResync_DropsTrailingJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, junked, cleanLen := junkedFixture(t, dir, make([]byte, 8))

	var skipped []DamagedRange
	out := filepath.Join(dir, "out.mkv")
	err := Reindex(ctx, junked, out, mkv.Options{
		Resync: true,
		OnSkip: func(r DamagedRange) { skipped = append(skipped, r) },
	})
	if err != nil {
		t.Fatalf("Reindex --resync on a junk-only file: %v", err)
	}
	if len(skipped) != 1 || skipped[0].StartOffset != cleanLen {
		t.Errorf("OnSkip = %+v, want the junk range from %d", skipped, cleanLen)
	}
	if n := blockReaderIteratesCleanly(t, out); n == 0 {
		t.Error("output does not decode")
	}
}

// TestReindex_StreamedSegmentWithJunkStaysStrict: with no declared Segment
// end there is no boundary to tell junk from content, so the strict walk
// keeps refusing - the junk drop must never fire on an unsealed file.
func TestReindex_StreamedSegmentWithJunkStaysStrict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, junked, _ := junkedFixture(t, dir, make([]byte, 8))
	streamed := filepath.Join(dir, "streamed.mkv")
	writeAll(t, streamed, makeSegmentStreamed(t, readAll(t, junked)))

	err := Reindex(ctx, streamed, filepath.Join(dir, "out.mkv"))
	if err == nil {
		t.Fatal("junk on a streamed (unknown-size) Segment must keep the strict refusal")
	}
	if !errors.Is(err, ErrCorruptSource) {
		t.Errorf("the strict refusal must wrap ErrCorruptSource, got: %v", err)
	}
}
