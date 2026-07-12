package ops

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// resealSegmentSize rewrites the Segment size of a hand-rebuilt fixture (the
// writer seals it on Finalize, but grafting bytes in afterwards makes the
// sealed value stale).
func resealSegmentSize(t *testing.T, path string) {
	t.Helper()
	data := readAll(t, path)
	br := bytes.NewReader(data)
	h1, n1, err := ebml.ReadElementHeader(br)
	if err != nil {
		t.Fatalf("reseal: EBML header: %v", err)
	}
	segIDOff := int64(n1) + h1.Size
	sb := bytes.NewReader(data[segIDOff:])
	h2, n2, err := ebml.ReadElementHeader(sb)
	if err != nil || h2.ID != mkv.IDSegment {
		t.Fatalf("reseal: Segment header: %v", err)
	}
	segDataStart := segIDOff + int64(n2)
	enc, err := vintEncode(int64(len(data))-segDataStart, n2-4)
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	copy(data[segIDOff+4:], enc)
	writeAll(t, path, data)
}

// delayedAudioFixture builds the repack defect: video blocks at ts, audio
// blocks delayMs later, per cluster.
func delayedAudioFixture(t *testing.T, dir, name string, n int, delayMs int64) string {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, n)
	for i := 0; i < n; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
			{TrackNumber: 2, Timecode: ts + delayMs, Keyframe: true, Data: []byte{0x01, 0x02}},
		})
	}
	return buildMultiClusterMKV(t, dir, name, tracks, sets, int64(n*1000))
}

// trackStartsMs walks the file and returns the minimum absolute block time
// per track, in ms.
func trackStartsMs(t *testing.T, path string) map[uint64]int64 {
	t.Helper()
	c, err := reader.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	starts := make(map[uint64]int64)
	for {
		b, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("block walk: %v", err)
		}
		if cur, ok := starts[b.TrackNumber]; !ok || b.Timecode < cur {
			starts[b.TrackNumber] = b.Timecode
		}
	}
	return starts
}

// TestRetime_CancelsAudioDelay: the flagship case - 900ms audio delay
// cancelled, payloads byte-identical, video untouched.
func TestRetime_CancelsAudioDelay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "delayed.mkv", 6, 900)
	origSize := int64(len(readAll(t, target)))
	pristine := filepath.Join(dir, "pristine.mkv")
	writeAll(t, pristine, readAll(t, target))

	before := trackStartsMs(t, target)
	if before[2]-before[1] != 900 {
		t.Fatalf("fixture defect = %dms, want 900", before[2]-before[1])
	}

	err := RetimeTracksInPlace(ctx, target, map[uint64]int64{2: -900_000_000}, mkv.Options{DeepVerify: true})
	if err != nil {
		t.Fatalf("RetimeTracksInPlace: %v", err)
	}

	after := trackStartsMs(t, target)
	if after[2] != after[1] {
		t.Errorf("audio starts at %dms, video at %dms - delay not cancelled", after[2], after[1])
	}
	if after[1] != before[1] {
		t.Errorf("video moved from %dms to %dms; it must not", before[1], after[1])
	}
	if got := int64(len(readAll(t, target))); got != origSize {
		t.Errorf("file size changed %d -> %d; in-place retime must not grow the file", origSize, got)
	}

	// Payloads byte-identical (only timecodes moved).
	diffs, err := CompareBlocks(ctx, pristine, target)
	if err != nil {
		t.Fatalf("CompareBlocks: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("payloads changed: %v", diffs)
	}
	validateErrorFree(t, target)
}

// TestRetime_MultiTrackDifferentShifts: the case the remux-based v1 refuses.
func TestRetime_MultiTrackDifferentShifts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2), audioTrack(3)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}},
			{TrackNumber: 3, Timecode: ts + 700, Keyframe: true, Data: []byte{0x02}},
		})
	}
	target := buildMultiClusterMKV(t, dir, "multi.mkv", tracks, sets, 4000)

	err := RetimeTracks(ctx, target, map[uint64]int64{
		2: -300_000_000,
		3: -700_000_000,
	}, mkv.Options{DeepVerify: true})
	if err != nil {
		t.Fatalf("RetimeTracks: %v", err)
	}
	after := trackStartsMs(t, target)
	if after[2] != after[1] || after[3] != after[1] {
		t.Errorf("starts after retime: video=%d a1=%d a2=%d, want all equal", after[1], after[2], after[3])
	}
}

// TestRetime_Refusals: unknown track, int16 overflow, negative absolute
// timestamp, sub-resolution shift - and the file must be untouched each time.
func TestRetime_Refusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "refuse.mkv", 4, 900)
	before := readAll(t, target)

	cases := []struct {
		name  string
		shift map[uint64]int64
		want  string
	}{
		{"unknown track", map[uint64]int64{9: -100_000_000}, "does not exist"},
		{"int16 overflow", map[uint64]int64{2: 40_000_000_000}, "int16"},
		{"negative absolute", map[uint64]int64{2: -1_500_000_000}, "negative"},
		{"sub-resolution", map[uint64]int64{2: -400_000}, "resolution"},
		{"empty", nil, "no tracks"},
	}
	for _, tc := range cases {
		err := RetimeTracks(ctx, target, tc.shift)
		if err == nil {
			t.Fatalf("%s: expected a refusal", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
	if !bytes.Equal(before, readAll(t, target)) {
		t.Error("a refused retime modified the file")
	}
}

// TestRetimeReplace_Rewrite: the sequential engine - delay cancelled through
// a verified atomic replacement, payloads intact, cues rebuilt healthy, and
// the rollback delta reconstructs the pre-retime original.
func TestRetimeReplace_Rewrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "replace.mkv", 6, 900)
	original := readAll(t, target)

	var delta bytes.Buffer
	err := RetimeTracksReplace(ctx, target, map[uint64]int64{2: -900_000_000}, mkv.Options{
		DeepVerify:       true,
		KeepBackup:       true,
		RollbackSink:     &delta,
		RollbackRequired: true,
	})
	if err != nil {
		t.Fatalf("RetimeTracksReplace: %v", err)
	}

	after := trackStartsMs(t, target)
	if after[2] != after[1] {
		t.Errorf("audio starts at %dms, video at %dms - delay not cancelled", after[2], after[1])
	}
	if !bytes.Equal(readAll(t, target+".bak"), original) {
		t.Error("KeepBackup must preserve the pre-retime original")
	}
	diffs, err := CompareBlocks(ctx, target+".bak", target)
	if err != nil {
		t.Fatalf("CompareBlocks: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("payloads changed: %v", diffs)
	}
	validateErrorFree(t, target)

	// The rebuilt index must be healthy (video-keyed).
	ch, err := CueHealth(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Healthy {
		t.Errorf("replace must rebuild a healthy index, got: %s", ch.Reason)
	}

	rollbackRoundTrip(t, target, delta.Bytes(), original)
}

// TestRetime_AutoScattersToReplace: on a tiny fixture the auto threshold
// always routes to the rewrite (in-place cannot beat rewriting a few KB);
// the result must be correct either way.
func TestRetime_AutoScattersToReplace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "auto.mkv", 6, 900)
	pristine := filepath.Join(dir, "pristine.mkv")
	writeAll(t, pristine, readAll(t, target))

	if err := RetimeTracks(ctx, target, map[uint64]int64{2: -900_000_000}, mkv.Options{DeepVerify: true}); err != nil {
		t.Fatalf("RetimeTracks auto: %v", err)
	}
	after := trackStartsMs(t, target)
	if after[2] != after[1] {
		t.Errorf("audio starts at %dms, video at %dms", after[2], after[1])
	}
	diffs, err := CompareBlocks(ctx, pristine, target)
	if err != nil || len(diffs) != 0 {
		t.Errorf("payloads must be intact (diffs=%v err=%v)", diffs, err)
	}
	validateErrorFree(t, target)
}

// TestRetimeReplace_UnknownSizeSegment: the rewrite lifts the in-place
// restriction on streamed (unknown-size) Segments - the class every mkvgo
// output before the size-sealing writer belongs to.
func TestRetimeReplace_UnknownSizeSegment(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "unsized.mkv", 4, 900)

	// Unseal the Segment size back to the unknown-size marker (8-byte all-ones).
	data := readAll(t, target)
	br := bytes.NewReader(data)
	h1, n1, err := ebml.ReadElementHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	segIDOff := int64(n1) + h1.Size
	copy(data[segIDOff+4:segIDOff+12], []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	writeAll(t, target, data)

	if err := RetimeTracksInPlace(ctx, target, map[uint64]int64{2: -900_000_000}); err == nil {
		t.Fatal("in-place must refuse an unknown-size Segment")
	} else if !strings.Contains(err.Error(), "unknown-size") {
		t.Errorf("refusal should name the cause, got: %v", err)
	}
	if err := RetimeTracksReplace(ctx, target, map[uint64]int64{2: -900_000_000}); err != nil {
		t.Fatalf("the rewrite must handle an unknown-size Segment: %v", err)
	}
	after := trackStartsMs(t, target)
	if after[2] != after[1] {
		t.Errorf("audio starts at %dms, video at %dms", after[2], after[1])
	}
	validateErrorFree(t, target)
}

// TestRetime_DeepVerifyPreexistingDefect: a file whose index was ALREADY
// defective (audio-keyed cues, the ffmpeg-muxer heritage) must not have a
// correct retime refused for it - the deep verify diffs the issue sets and
// only an ADDED error blocks. The preexisting defect is reported through
// OnPreexisting; StrictVerify restores the absolute refusal.
func TestRetime_DeepVerifyPreexistingDefect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 500, Keyframe: true, Data: []byte{0x01}},
		})
	}
	// Cues keyed on the AUDIO track: validate reports the error-severity
	// "cues-non-video" defect before the retime ever runs.
	build := func(name string) string {
		return buildMKVWithCues(t, dir, name, tracks, sets, []mkv.CuePoint{
			{TimeMs: 500, Track: 2, ClusterPos: 100},
			{TimeMs: 1500, Track: 2, ClusterPos: 200},
		})
	}

	target := build("preexisting.mkv")
	var preexisting []mkv.Issue
	err := RetimeTracksInPlace(ctx, target, map[uint64]int64{2: -500_000_000}, mkv.Options{
		DeepVerify:    true,
		OnPreexisting: func(is mkv.Issue) { preexisting = append(preexisting, is) },
	})
	if err != nil {
		t.Fatalf("a correct retime must not be refused for a PREEXISTING defect: %v", err)
	}
	found := false
	for _, is := range preexisting {
		if is.Code == "cues-non-video" {
			found = true
		}
	}
	if !found {
		t.Errorf("the preexisting defect must be reported through OnPreexisting, got %+v", preexisting)
	}

	strictTarget := build("strict.mkv")
	before := readAll(t, strictTarget)
	err = RetimeTracksInPlace(ctx, strictTarget, map[uint64]int64{2: -500_000_000}, mkv.Options{
		DeepVerify:   true,
		StrictVerify: true,
	})
	if err == nil {
		t.Fatal("StrictVerify must refuse on any error-severity issue, preexisting included")
	}
	if !bytes.Equal(before, readAll(t, strictTarget)) {
		t.Error("the strict refusal must roll the file back byte-identical")
	}
}

// noSyncFile hides a handle's Sync method while keeping Truncate: the shape
// of a port whose durability semantics are undeclared.
type noSyncFile struct {
	mkv.ReadWriteSeekCloser
	trunc interface{ Truncate(size int64) error }
}

func (n *noSyncFile) Truncate(size int64) error { return n.trunc.Truncate(size) }

// TestInPlaceRequiresSync: a handle that cannot Sync must be refused BEFORE
// any patch - the crash-safety journal is worthless without the write
// barrier, and degrading silently would be worse than not patching at all.
// Covers both in-place operations (shared prologue) and the journal rollback.
func TestInPlaceRequiresSync(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "nosync.mkv", 4, 900)
	before := readAll(t, target)

	fs := &mkv.FS{
		OpenFile: func(path string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
			f, err := os.OpenFile(path, flag, perm)
			if err != nil {
				return nil, err
			}
			return &noSyncFile{ReadWriteSeekCloser: f, trunc: f}, nil
		},
	}

	err := RetimeTracks(ctx, target, map[uint64]int64{2: -900_000_000}, mkv.Options{FS: fs})
	if err == nil {
		t.Fatal("RetimeTracks must refuse a handle without Sync")
	}
	if !strings.Contains(err.Error(), "Sync") {
		t.Errorf("refusal should name the missing capability, got: %v", err)
	}
	if err := ReindexInPlace(ctx, target, mkv.Options{FS: fs}); err == nil || !strings.Contains(err.Error(), "Sync") {
		t.Errorf("ReindexInPlace must refuse a handle without Sync, got: %v", err)
	}
	if !bytes.Equal(before, readAll(t, target)) {
		t.Error("the refusal must leave the file untouched")
	}
}

// TestRetime_CrashRecovery: an interrupted run (journal landed, patches
// half-applied, no truncate) must be rolled back byte-identical by the
// auto-recovery of the next in-place operation.
func TestRetime_CrashRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "crash.mkv", 4, 900)
	original := readAll(t, target)

	// Simulate the crash by hand with the same internals: scan, land the
	// journal, apply only half the patches, and stop.
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(original))
	patches, _, err := retimeScan(ctx, f, size, map[uint64]int64{2: -900}, nil, 0)
	if err != nil {
		t.Fatalf("retimeScan: %v", err)
	}
	if len(patches) < 2 {
		t.Fatalf("need at least 2 patches, got %d", len(patches))
	}
	zones := make([]inplaceZone, len(patches))
	for i, p := range patches {
		zones[i] = inplaceZone{off: p.off, orig: p.orig}
	}
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := writeInPlaceJournal(f, size, zones); err != nil {
		t.Fatal(err)
	}
	for _, p := range patches[:len(patches)/2] {
		if _, err := f.Seek(p.off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(p.repl); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	recovered, err := RecoverInPlace(ctx, target)
	if err != nil {
		t.Fatalf("RecoverInPlace: %v", err)
	}
	if !recovered {
		t.Fatal("expected the journal to be found and rolled back")
	}
	if !bytes.Equal(original, readAll(t, target)) {
		t.Error("recovery did not restore the original bytes")
	}
}

// TestRetime_RollbackDelta: the journal-as-delta synergy - a retime plus its
// rollback delta must reconstruct the pre-retime file exactly.
func TestRetime_RollbackDelta(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "delta.mkv", 6, 900)
	original := readAll(t, target)

	var delta bytes.Buffer
	err := RetimeTracksInPlace(ctx, target, map[uint64]int64{2: -900_000_000}, mkv.Options{
		RollbackSink:     &delta,
		RollbackRequired: true,
	})
	if err != nil {
		t.Fatalf("RetimeTracks with RollbackSink: %v", err)
	}
	if delta.Len() == 0 || delta.Len() > 1024 {
		t.Errorf("delta is %d bytes, want a fixed few hundred (tiny patches + entry framing)", delta.Len())
	}
	rollbackRoundTrip(t, target, delta.Bytes(), original)
}

// TestRetime_ClusterCRCRecomputed: a cluster carrying a CRC-32 first child
// must have it recomputed after its block timecodes changed.
func TestRetime_ClusterCRCRecomputed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	target := delayedAudioFixture(t, dir, "crc.mkv", 3, 900)
	data := readAll(t, target)

	// Graft a CRC-32 first child into cluster 2 by rebuilding the file:
	// [.. cluster1][cluster2' = hdr | BF 84 crc | old body][cluster3 ..].
	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)
	if len(offsets) < 3 {
		t.Fatalf("fixture has only %d clusters", len(offsets))
	}
	cl := offsets[1]
	// Parse cluster 2's header (4-byte ID + 1-byte size on this fixture).
	if data[cl+4]&0x80 == 0 {
		t.Fatalf("expected 1-byte cluster size VINT")
	}
	oldSize := int64(data[cl+4] & 0x7F)
	body := data[cl+5 : cl+5+oldSize]
	crcVal := make([]byte, 4)
	binary.LittleEndian.PutUint32(crcVal, crc32.ChecksumIEEE(body))
	newBody := append([]byte{0xBF, 0x84}, crcVal...)
	newBody = append(newBody, body...)
	if int64(len(newBody)) > 0x7E {
		t.Fatalf("fixture cluster too big for a 1-byte size VINT")
	}
	rebuilt := append([]byte(nil), data[:cl+4]...)
	rebuilt = append(rebuilt, byte(0x80|len(newBody)))
	rebuilt = append(rebuilt, newBody...)
	rebuilt = append(rebuilt, data[cl+5+oldSize:]...)
	crcPath := filepath.Join(dir, "crc.grafted.mkv")
	writeAll(t, crcPath, rebuilt)
	resealSegmentSize(t, crcPath)

	// The graft shifted every later offset: the old Cues/SeekHead are stale.
	// Retime only walks clusters and cues by structure, so that is fine for
	// this test; verify the CRC is valid before and after.
	if err := RetimeTracks(ctx, crcPath, map[uint64]int64{2: -900_000_000}); err != nil {
		t.Fatalf("RetimeTracks: %v", err)
	}
	got := readAll(t, crcPath)
	gCl := findAll(got, magic)[1]
	gLen := int64(got[gCl+4] & 0x7F)
	gBody := got[gCl+5 : gCl+5+gLen]
	if !bytes.Equal(gBody[0:2], []byte{0xBF, 0x84}) {
		t.Fatal("CRC child vanished")
	}
	want := crc32.ChecksumIEEE(gBody[6:])
	if binary.LittleEndian.Uint32(gBody[2:6]) != want {
		t.Error("cluster CRC-32 was not recomputed over the patched body")
	}
}

// TestRetime_AudioOnlyCuesShift: an audio-only file cues audio blocks; those
// CueTimes must move with the track.
func TestRetime_AudioOnlyCuesShift(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{audioTrack(1)}
	sets := make([][]mkv.Block, 0, 6)
	for i := 0; i < 6; i++ {
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: int64(i*1000) + 500, Keyframe: true, Data: []byte{0x01, 0x02}},
		})
	}
	target := buildMultiClusterMKV(t, dir, "audio.mkv", tracks, sets, 6500)

	before, err := reader.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Cues) == 0 {
		t.Skip("fixture produced no audio cues")
	}

	if err := RetimeTracks(ctx, target, map[uint64]int64{1: -500_000_000}, mkv.Options{DeepVerify: true}); err != nil {
		t.Fatalf("RetimeTracks: %v", err)
	}
	after, err := reader.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Cues) != len(before.Cues) {
		t.Fatalf("cue count changed %d -> %d", len(before.Cues), len(after.Cues))
	}
	for i := range after.Cues {
		if want := before.Cues[i].TimeMs - 500; after.Cues[i].TimeMs != want {
			t.Errorf("cue %d time = %dms, want %dms", i, after.Cues[i].TimeMs, want)
		}
	}
	validateErrorFree(t, target)
}
