package mp4

// mutation_kill_test.go — targeted tests for surviving gremlins mutants.
// Each test exercises an exact boundary value or asserts an exact arithmetic
// result so that the specific operator the mutant flips produces a clearly
// different observable output.

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ── parse.go: iterBoxes boundaries ─────────────────────────────────────────

// TestIterBoxesExact8BytesBoundary kills the off+8 <= len(buf) CONDITIONALS_BOUNDARY
// mutant.  A buffer of exactly 8 bytes (one zero-payload box) must produce one
// box; flipping <= to < would skip the last valid header.
func TestIterBoxesExact8BytesBoundary(t *testing.T) {
	buf := []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}
	boxes, err := iterBoxes(buf)
	if err != nil || len(boxes) != 1 {
		t.Fatalf("8-byte buf: err=%v boxes=%d, want 1 box", err, len(boxes))
	}
	if boxes[0].typ != "free" || len(boxes[0].payload) != 0 {
		t.Errorf("box = {%q, %d bytes}, want {free, 0}", boxes[0].typ, len(boxes[0].payload))
	}
}

// TestIterBoxes7BytesIsEmpty confirms a 7-byte buffer is below the 8-byte
// header minimum: 0 boxes, no error (the loop guard simply does not fire).
func TestIterBoxes7BytesIsEmpty(t *testing.T) {
	boxes, err := iterBoxes(make([]byte, 7))
	if err != nil || len(boxes) != 0 {
		t.Errorf("7-byte buf: err=%v boxes=%d, want 0", err, len(boxes))
	}
}

// TestIterBoxes64BitBoxBoundary exercises the largesize (size==1) path.
// Exactly 16 bytes must parse successfully; 15 bytes must error because the
// 8-byte largesize field is truncated.
func TestIterBoxes64BitBoxBoundary(t *testing.T) {
	var buf [16]byte
	binary.BigEndian.PutUint32(buf[0:4], 1)   // size==1 → 64-bit largesize
	copy(buf[4:8], "test")                     // type
	binary.BigEndian.PutUint64(buf[8:16], 16)  // largesize = 16 (header only, no payload)
	boxes, err := iterBoxes(buf[:])
	if err != nil || len(boxes) != 1 || len(boxes[0].payload) != 0 {
		t.Fatalf("64-bit box 16B: err=%v boxes=%d payload=%d", err, len(boxes), len(boxes[0].payload))
	}
	if _, err2 := iterBoxes(buf[:15]); err2 == nil {
		t.Error("15-byte 64-bit box must error (largesize truncated)")
	}
}

// TestIterBoxesSize0FillsToEnd exercises the size==0 path: the box spans to
// the end of the container.
func TestIterBoxesSize0FillsToEnd(t *testing.T) {
	buf := []byte{0, 0, 0, 0, 'm', 'd', 'a', 't', 0xAA, 0xBB, 0xCC}
	boxes, err := iterBoxes(buf)
	if err != nil || len(boxes) != 1 {
		t.Fatalf("size-0 box: err=%v boxes=%d, want 1", err, len(boxes))
	}
	if !bytes.Equal(boxes[0].payload, []byte{0xAA, 0xBB, 0xCC}) {
		t.Errorf("payload = % x, want aa bb cc", boxes[0].payload)
	}
}

// TestIterBoxesMinSizeIsHeader validates that size==hdr (8 bytes, zero-payload)
// is valid, while size==hdr-1 is rejected. Kills the size < int64(hdr)
// CONDITIONALS_BOUNDARY mutant (< to <=).
func TestIterBoxesMinSizeIsHeader(t *testing.T) {
	// size=8 → valid
	valid := []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'}
	if _, err := iterBoxes(valid); err != nil {
		t.Errorf("size==hdr should be valid: %v", err)
	}
	// size=7 → invalid
	invalid := []byte{0, 0, 0, 7, 'f', 'r', 'e', 'e'}
	if _, err := iterBoxes(invalid); err == nil {
		t.Error("size<hdr must error")
	}
}

// ── parse.go: headerFrameRate ───────────────────────────────────────────────

// TestHeaderFrameRateExact16Bytes kills the len(stts.payload) < 16
// CONDITIONALS_BOUNDARY mutant.  Exactly 16 bytes must give a valid fps; 15
// must return 0.
func TestHeaderFrameRateExact16Bytes(t *testing.T) {
	// stts: version+flags(4) entry_count(4) sample_count(4) sample_delta(4)
	var p bw
	p.u32(0)    // version/flags
	p.u32(1)    // entry_count
	p.u32(100)  // sample_count
	p.u32(3000) // sample_delta → fps = 90000/3000 = 30

	boxes := []memBox{{typ: "stts", payload: p.b}}
	if fps := headerFrameRate(boxes, 90000); fps != 30.0 {
		t.Errorf("16-byte stts fps = %v, want 30", fps)
	}
	boxes15 := []memBox{{typ: "stts", payload: p.b[:15]}}
	if fps := headerFrameRate(boxes15, 90000); fps != 0 {
		t.Errorf("15-byte stts fps = %v, want 0", fps)
	}
}

// TestHeaderFrameRateDeltaZeroAndNoTimescale confirms that delta==0 or
// timescale==0 both return 0 (no divide-by-zero).
func TestHeaderFrameRateDeltaZeroAndNoTimescale(t *testing.T) {
	var p bw
	p.u32(0); p.u32(1); p.u32(1); p.u32(0) // delta=0
	if fps := headerFrameRate([]memBox{{typ: "stts", payload: p.b}}, 90000); fps != 0 {
		t.Errorf("delta=0 → %v, want 0", fps)
	}
	var p2 bw
	p2.u32(0); p2.u32(1); p2.u32(1); p2.u32(1001) // delta=1001
	if fps := headerFrameRate([]memBox{{typ: "stts", payload: p2.b}}, 0); fps != 0 {
		t.Errorf("timescale=0 → %v, want 0", fps)
	}
}

// ── parse.go: headerFrameCount ──────────────────────────────────────────────

// TestHeaderFrameCountExact12Bytes kills the len(stsz.payload) >= 12
// CONDITIONALS_BOUNDARY mutant.  Exactly 12 bytes must return count; 11 must
// return 0.
func TestHeaderFrameCountExact12Bytes(t *testing.T) {
	var p bw
	p.u32(0)  // version/flags
	p.u32(0)  // sample_size=0
	p.u32(77) // sample_count=77

	if count := headerFrameCount([]memBox{{typ: "stsz", payload: p.b}}); count != 77 {
		t.Errorf("12-byte stsz count = %d, want 77", count)
	}
	if count := headerFrameCount([]memBox{{typ: "stsz", payload: p.b[:11]}}); count != 0 {
		t.Errorf("11-byte stsz count = %d, want 0", count)
	}
}

// ── parse.go: mdhdDurationMs ────────────────────────────────────────────────

// TestMdhdDurationMsSentinelsAndArithmetic kills CONDITIONALS_NEGATION mutants
// on ticks==0 and ticks==0xFFFFFFFF, the CONDITIONALS_BOUNDARY mutant on
// ticks>=1<<62, and ARITHMETIC_BASE mutants on ticks*1000/ts.
func TestMdhdDurationMsSentinelsAndArithmetic(t *testing.T) {
	v0 := func(ts, dur uint32) []byte {
		var w bw
		w.u32(0); w.u32(0); w.u32(0)
		w.u32(ts); w.u32(dur); w.u16(0); w.u16(0)
		return w.b
	}
	v1 := func(ts uint32, dur uint64) []byte {
		var w bw
		w.u8(1); w.u24(0)
		w.u64(0); w.u64(0) // creation/modification
		w.u32(ts); w.u64(dur); w.u16(0); w.u16(0)
		return w.b
	}

	// ticks==0 → must return 0.
	if d := mdhdDurationMs(v0(1000, 0)); d != 0 {
		t.Error("ticks=0 must return 0")
	}
	// ticks==0xFFFFFFFF (v0 sentinel) → must return 0.
	if d := mdhdDurationMs(v0(1000, 0xFFFFFFFF)); d != 0 {
		t.Error("ticks=0xFFFFFFFF must return 0")
	}
	// ticks==0xFFFFFFFE is NOT a sentinel → must return non-zero.
	if d := mdhdDurationMs(v0(1000, 0xFFFFFFFE)); d == 0 {
		t.Error("ticks=0xFFFFFFFE should return non-zero")
	}
	// ticks==1<<62 (v1 overflow guard) → must return 0.
	if d := mdhdDurationMs(v1(1000, 1<<62)); d != 0 {
		t.Errorf("ticks=1<<62 must return 0, got %d", d)
	}
	// ticks==(1<<62)-1 is just below the guard → must return non-zero.
	if d := mdhdDurationMs(v1(1000, (1<<62)-1)); d == 0 {
		t.Error("ticks=(1<<62)-1 should return non-zero")
	}
	// Arithmetic: 6000 ticks at ts=2 → 6000*1000/2 = 3000000.
	if d := mdhdDurationMs(v0(2, 6000)); d != 3000000 {
		t.Errorf("6000ticks@ts=2 → %d, want 3000000", d)
	}
	// Arithmetic: 90000 ticks at ts=90000 → 1000 ms.
	if d := mdhdDurationMs(v0(90000, 90000)); d != 1000 {
		t.Errorf("90000ticks@ts=90000 → %d, want 1000", d)
	}
}

// ── sampletable.go: ticksToMs arithmetic ───────────────────────────────────

// TestTicksToMsArithmeticDistinct kills ARITHMETIC_BASE mutants (* → +, etc.)
// by asserting the exact integer-division result for non-trivial inputs.
func TestTicksToMsArithmeticDistinct(t *testing.T) {
	// 3001 * 1000 / 1001 = 2998 (integer division — distinguishes +, -, /)
	if got := ticksToMs(3001, 1001); got != 2998 {
		t.Errorf("ticksToMs(3001,1001) = %d, want 2998", got)
	}
	// 9 * 1000 / 3 = 3000
	if got := ticksToMs(9, 3); got != 3000 {
		t.Errorf("ticksToMs(9,3) = %d, want 3000", got)
	}
}

// ── sampletable.go: parseStsz ──────────────────────────────────────────────

// TestParseStszUniformBoundary kills the sampleSize != 0 CONDITIONALS_NEGATION
// mutant.  A non-zero uniform sampleSize must fill every entry with that value;
// the mutation (sampleSize==0) would try to parse missing individual sizes.
func TestParseStszUniformBoundary(t *testing.T) {
	var w bw
	w.u32(0)   // version/flags
	w.u32(512) // sampleSize=512 (uniform)
	w.u32(4)   // count=4

	sizes, err := parseStsz(w.b)
	if err != nil || len(sizes) != 4 {
		t.Fatalf("uniform stsz: err=%v len=%d, want 4", err, len(sizes))
	}
	for i, s := range sizes {
		if s != 512 {
			t.Errorf("sizes[%d] = %d, want 512", i, s)
		}
	}
}

// TestParseStszMaxSamplesBoundary kills the count > maxSamples
// CONDITIONALS_BOUNDARY mutant (> vs >=).  count==maxSamples must succeed;
// count==maxSamples+1 must error.
func TestParseStszMaxSamplesBoundary(t *testing.T) {
	var wOk bw
	wOk.u32(0); wOk.u32(1) // version/flags, sampleSize=1 (uniform, no size array)
	wOk.u32(maxSamples)
	if _, err := parseStsz(wOk.b); err != nil {
		t.Errorf("count==maxSamples should succeed: %v", err)
	}

	var wErr bw
	wErr.u32(0); wErr.u32(1)
	wErr.u32(maxSamples + 1)
	if _, err := parseStsz(wErr.b); err == nil {
		t.Error("count==maxSamples+1 must error")
	}
}

// TestParseStszVariableSizesIndexArithmetic verifies the 12+i*4 index
// arithmetic for per-sample sizes by checking the exact value at a specific
// position.  Kills ARITHMETIC_BASE mutants on the index expression.
func TestParseStszVariableSizesIndexArithmetic(t *testing.T) {
	var w bw
	w.u32(0) // version/flags
	w.u32(0) // sampleSize=0 → individual sizes follow
	w.u32(3) // count=3
	w.u32(11)
	w.u32(22)
	w.u32(33)

	sizes, err := parseStsz(w.b)
	if err != nil || len(sizes) != 3 {
		t.Fatalf("variable stsz: err=%v len=%d", err, len(sizes))
	}
	if sizes[0] != 11 || sizes[1] != 22 || sizes[2] != 33 {
		t.Errorf("sizes = %v, want [11 22 33]", sizes)
	}
}

// ── sampletable.go: samplesForChunk ────────────────────────────────────────

// TestSamplesForChunkExactBoundary kills the c >= e.firstChunk
// CONDITIONALS_BOUNDARY mutant (>= vs >).  c==firstChunk must use that entry's
// perChunk; with the mutation (c>firstChunk), c==firstChunk would not match,
// returning 0 instead.
func TestSamplesForChunkExactBoundary(t *testing.T) {
	entries := []stscEntry{
		{firstChunk: 1, perChunk: 8},
		{firstChunk: 4, perChunk: 3},
	}
	// c==firstChunk of first entry.
	if got := samplesForChunk(1, entries); got != 8 {
		t.Errorf("c==1 (first entry): %d, want 8", got)
	}
	// c==firstChunk of second entry.
	if got := samplesForChunk(4, entries); got != 3 {
		t.Errorf("c==4 (second entry): %d, want 3", got)
	}
	// c between entries uses first entry's count.
	if got := samplesForChunk(3, entries); got != 8 {
		t.Errorf("c=3 (between): %d, want 8", got)
	}
	// c before any entry returns 0.
	if got := samplesForChunk(0, entries); got != 0 {
		t.Errorf("c=0 (before any): %d, want 0", got)
	}
}

// ── sampletable.go: parseStts ──────────────────────────────────────────────

// TestParseSttsRunCapAtN kills the idx < n CONDITIONALS_BOUNDARY mutant.  A
// run larger than n must be capped at n; flipping idx < n to idx <= n would
// allow an extra write past the end of durations, causing a panic.
func TestParseSttsRunCapAtN(t *testing.T) {
	var p bw
	p.u32(0)  // version/flags
	p.u32(1)  // entry_count=1
	p.u32(10) // run=10 (> n=3)
	p.u32(40) // delta=40

	durs, err := parseStts(p.b, 3)
	if err != nil || len(durs) != 3 {
		t.Fatalf("stts run cap: err=%v len=%d, want 3", err, len(durs))
	}
	for i, d := range durs {
		if d != 40 {
			t.Errorf("durs[%d] = %d, want 40", i, d)
		}
	}
}

// TestParseSttsExactRunMatchesN verifies the run exactly filling n samples.
func TestParseSttsExactRunMatchesN(t *testing.T) {
	var p bw
	p.u32(0); p.u32(1); p.u32(5); p.u32(33) // run=5, delta=33, n=5

	durs, err := parseStts(p.b, 5)
	if err != nil || len(durs) != 5 {
		t.Fatalf("stts exact run: err=%v len=%d", err, len(durs))
	}
	for _, d := range durs {
		if d != 33 {
			t.Errorf("duration %d != 33", d)
		}
	}
}

// ── sampletable.go: parseStss ──────────────────────────────────────────────

// TestParseStssCountAndTruncation verifies stss parses correct sync numbers
// and returns nil for a truncated payload.
func TestParseStssCountAndTruncation(t *testing.T) {
	var p bw
	p.u32(0) // version/flags
	p.u32(3) // count=3
	p.u32(1); p.u32(5); p.u32(9)

	set := parseStss([]memBox{{typ: "stss", payload: p.b}})
	if !set[1] || !set[5] || !set[9] || len(set) != 3 {
		t.Errorf("stss set = %v, want {1,5,9}", set)
	}

	// Payload claims 4 entries but only has 3 worth → nil (defensive).
	var p2 bw
	p2.u32(0); p2.u32(4)
	p2.u32(1); p2.u32(5); p2.u32(9)
	if parseStss([]memBox{{typ: "stss", payload: p2.b}}) != nil {
		t.Error("truncated stss must return nil")
	}
}

// ── audio.go: findSync boundaries ──────────────────────────────────────────

// TestFindSyncExact2BytesBoundary kills the len(frame) < 2
// CONDITIONALS_BOUNDARY mutant.  A 2-byte frame containing the syncword must
// return 0; flipping < to <= would return -1 instead.
func TestFindSyncExact2BytesBoundary(t *testing.T) {
	frame2 := []byte{0x0B, 0x77} // exactly the syncword
	if got := findSync(frame2, 0x0B77, 16); got != 0 {
		t.Errorf("2-byte syncword: got %d, want 0", got)
	}
	// 1-byte frame must return -1.
	if got := findSync([]byte{0x0B}, 0x0B77, 16); got != -1 {
		t.Errorf("1-byte frame: got %d, want -1", got)
	}
}

// TestFindSyncMaxScanBoundary kills the i <= maxScan CONDITIONALS_BOUNDARY
// mutant.  The syncword at exactly maxScan must be found; one past must not.
func TestFindSyncMaxScanBoundary(t *testing.T) {
	// syncword at index 3; frame has 6 bytes total.
	frame := []byte{0xAA, 0xBB, 0xCC, 0x0B, 0x77, 0xFF}
	// maxScan=3: checks i=0,1,2,3 → finds syncword.
	if got := findSync(frame, 0x0B77, 3); got != 3 {
		t.Errorf("syncword at maxScan: got %d, want 3", got)
	}
	// maxScan=2: syncword at i=3 is beyond maxScan → -1.
	if got := findSync(frame, 0x0B77, 2); got != -1 {
		t.Errorf("syncword past maxScan: got %d, want -1", got)
	}
}

// ── audio.go: aacChannelsFrom boundary ─────────────────────────────────────

// TestAACChannelsFromBoundary kills the cc >= uint32(len(aacConfigChannels))
// CONDITIONALS_BOUNDARY mutant.  cc==7 (last valid index, len-1) must return
// the table value 8; cc==8 (== len) must return 0.
func TestAACChannelsFromBoundary(t *testing.T) {
	// aacConfigChannels = [8]uint8{0,1,2,3,4,5,6,8}, len=8
	if got := aacChannelsFrom(7, false); got != 8 {
		t.Errorf("cc=7 (last valid): %d, want 8", got)
	}
	if got := aacChannelsFrom(8, false); got != 0 {
		t.Errorf("cc=8 (== len): %d, want 0", got)
	}
	if got := aacChannelsFrom(0, false); got != 0 {
		t.Errorf("cc=0 (program config): %d, want 0", got)
	}
}

// ── audio.go: bitsLeft arithmetic ──────────────────────────────────────────

// TestBitsLeftArithmetic kills ARITHMETIC_BASE mutants on len(data)*8 - pos.
func TestBitsLeftArithmetic(t *testing.T) {
	// 5 bytes × 8 bits − 8 consumed = 32.
	r := &bitReader{data: make([]byte, 5), pos: 8}
	if got := bitsLeft(r); got != 32 {
		t.Errorf("5B pos=8: bitsLeft = %d, want 32", got)
	}
	// 3 bytes × 8 − 0 = 24.
	r2 := &bitReader{data: make([]byte, 3), pos: 0}
	if got := bitsLeft(r2); got != 24 {
		t.Errorf("3B pos=0: bitsLeft = %d, want 24", got)
	}
	// exactly exhausted: 2 bytes − 16 bits = 0.
	r3 := &bitReader{data: make([]byte, 2), pos: 16}
	if got := bitsLeft(r3); got != 0 {
		t.Errorf("2B pos=16: bitsLeft = %d, want 0", got)
	}
}

// ── demux.go: videoFrameRate ────────────────────────────────────────────────

// TestVideoFrameRateExact2Samples kills the len(samples) < 2
// CONDITIONALS_BOUNDARY mutant.  2 samples must give a positive fps (25); 1
// sample must return 0.
func TestVideoFrameRateExact2Samples(t *testing.T) {
	two := []inSample{{durMs: 40}, {durMs: 40}}
	if fps := videoFrameRate(two); fps != 25.0 {
		t.Errorf("2 samples at 40ms: fps = %v, want 25", fps)
	}
	if fps := videoFrameRate([]inSample{{durMs: 40}}); fps != 0 {
		t.Errorf("1 sample: fps = %v, want 0", fps)
	}
}

// TestVideoFrameRateTotalZero confirms that all-zero durations return 0 fps
// (total <= 0 guard).
func TestVideoFrameRateTotalZero(t *testing.T) {
	if fps := videoFrameRate([]inSample{{durMs: 0}, {durMs: 0}}); fps != 0 {
		t.Errorf("zero durations: fps = %v, want 0", fps)
	}
}

// ── demux.go: decodeTx3g boundaries ────────────────────────────────────────

// TestDecodeTx3gAllBoundaries kills multiple CONDITIONALS_BOUNDARY mutants in
// decodeTx3g: len(data)<2, n==0, and 2+n>len(data).
func TestDecodeTx3gAllBoundaries(t *testing.T) {
	// len(data) < 2 → false.
	if _, ok := decodeTx3g(nil); ok {
		t.Error("nil: want false")
	}
	if _, ok := decodeTx3g([]byte{0}); ok {
		t.Error("1-byte: want false")
	}
	// n == 0 → false.
	if _, ok := decodeTx3g([]byte{0, 0}); ok {
		t.Error("n=0: want false")
	}
	// 2+n == len(data) exactly (valid, returns text).
	data := []byte{0, 5, 'H', 'e', 'l', 'l', 'o'}
	got, ok := decodeTx3g(data)
	if !ok || string(got) != "Hello" {
		t.Errorf("exact: got=%q ok=%v, want Hello/true", got, ok)
	}
	// 2+n > len(data) → false.
	if _, ok := decodeTx3g([]byte{0, 10, 'H', 'i'}); ok {
		t.Error("truncated: want false")
	}
}

// ── demux.go: minTimecode ───────────────────────────────────────────────────

// TestMinTimecodePicksMinimum verifies minTimecode returns the block with the
// smallest Timecode.
func TestMinTimecodePicksMinimum(t *testing.T) {
	blocks := []mkv.Block{{Timecode: 500}, {Timecode: 100}, {Timecode: 300}}
	if got := minTimecode(blocks); got != 100 {
		t.Errorf("minTimecode = %d, want 100", got)
	}
	if got := minTimecode([]mkv.Block{{Timecode: 999}}); got != 999 {
		t.Errorf("single block: %d, want 999", got)
	}
	// First element is smallest.
	if got := minTimecode([]mkv.Block{{Timecode: 1}, {Timecode: 2}}); got != 1 {
		t.Errorf("first-min: %d, want 1", got)
	}
	// Last element is smallest.
	if got := minTimecode([]mkv.Block{{Timecode: 5}, {Timecode: 3}}); got != 3 {
		t.Errorf("last-min: %d, want 3", got)
	}
}

// ── mux.go: needCo64 exact boundary ────────────────────────────────────────

// TestNeedCo64ExactBoundary kills the CONDITIONALS_BOUNDARY mutant
// (> vs >=) on mdatDataLen > 0xFFFFFFFF-(256<<20).
func TestNeedCo64ExactBoundary(t *testing.T) {
	const threshold = int64(0xFFFFFFFF - (256 << 20))
	// At the threshold: NOT strictly greater → must be false.
	if needCo64(threshold) {
		t.Errorf("at threshold %d: must not need co64 (not strictly greater)", threshold)
	}
	// One above: strictly greater → must be true.
	if !needCo64(threshold + 1) {
		t.Errorf("threshold+1: must need co64")
	}
	// One below: not greater → false.
	if needCo64(threshold - 1) {
		t.Errorf("threshold-1: must not need co64")
	}
}

// ── subtitle.go: truncateRunes boundaries ──────────────────────────────────

// TestTruncateRunesAllBoundaries adds max=0 (the missing case from the
// existing test) and max==len(b) to kill the max > 0 and max < len(b)
// CONDITIONALS_BOUNDARY mutants.
func TestTruncateRunesAllBoundaries(t *testing.T) {
	// max=0: loop guard max > 0 must prevent any decrement → return 0.
	if n := truncateRunes([]byte("hello"), 0); n != 0 {
		t.Errorf("max=0: %d, want 0", n)
	}
	// max == len(b): loop guard max < len(b) is false → return len(b).
	if n := truncateRunes([]byte("hello"), 5); n != 5 {
		t.Errorf("max==len: %d, want 5", n)
	}
	// Multibyte rune "aé" (3 bytes): max=2 is mid-rune → back off to 1.
	ae := []byte("aé")
	if n := truncateRunes(ae, 2); n != 1 {
		t.Errorf("mid-rune max=2: %d, want 1", n)
	}
	// max=3 is exactly on-boundary → 3.
	if n := truncateRunes(ae, 3); n != 3 {
		t.Errorf("on-boundary max=3: %d, want 3", n)
	}
}

// ── chapters.go: parseChpl ─────────────────────────────────────────────────

// TestParseChplExact18BytesBoundary kills the pos+9 > len(payload)
// CONDITIONALS_BOUNDARY mutant.  A payload of exactly 18 bytes (9 header + 9
// for one entry with a zero-length title) must yield one chapter; 17 must give
// zero.
func TestParseChplExact18BytesBoundary(t *testing.T) {
	// 9-byte header: version(4)+reserved(4)+count(1)
	// 9-byte entry: start(8)+titleLen(1) with titleLen=0.
	var p []byte
	p = append(p, 1, 0, 0, 0) // version/flags
	p = append(p, 0, 0, 0, 0) // reserved
	p = append(p, 1)           // count=1
	startBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(startBytes, 1000000) // 100ms * 10000
	p = append(p, startBytes...)
	p = append(p, 0) // titleLen=0
	// total = 18 bytes

	chs := parseChpl(p)
	if len(chs) != 1 {
		t.Fatalf("18-byte chpl: %d chapters, want 1", len(chs))
	}
	if chs[0].StartMs != 100 {
		t.Errorf("start = %d, want 100", chs[0].StartMs)
	}
	// 17 bytes → entry is truncated (pos+9 > len) → 0 chapters.
	if chs17 := parseChpl(p[:17]); len(chs17) != 0 {
		t.Errorf("17-byte chpl: %d chapters, want 0", len(chs17))
	}
}

// TestParseChplStart100nsArithmetic kills ARITHMETIC_BASE mutants on
// start100ns / 10000.  Directly crafted raw bytes avoid the buildChpl
// path.
func TestParseChplStart100nsArithmetic(t *testing.T) {
	// 500ms = 5000000 100-nanosecond units.
	var p []byte
	p = append(p, 1, 0, 0, 0) // version/flags
	p = append(p, 0, 0, 0, 0) // reserved
	p = append(p, 1)           // count
	startBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(startBytes, 5000000)
	p = append(p, startBytes...)
	p = append(p, 5)
	p = append(p, 'H', 'e', 'l', 'l', 'o')

	chs := parseChpl(p)
	if len(chs) != 1 || chs[0].StartMs != 500 {
		t.Errorf("500ms chapter: %v, want StartMs=500", chs)
	}
}

// TestBuildChplMs100nsArithmetic kills ARITHMETIC_BASE mutants on
// uint64(start) * 10000.
func TestBuildChplMs100nsArithmetic(t *testing.T) {
	chs := []mkv.Chapter{{StartMs: 200, Title: "X"}}
	b := buildChpl(chs)
	payload := b[8:] // strip box header
	// Entry at offset 9 (version/flags=4 + reserved=4 + count=1).
	start100ns := binary.BigEndian.Uint64(payload[9:17])
	if start100ns != 2000000 { // 200ms * 10000 = 2000000
		t.Errorf("200ms → %d 100ns units, want 2000000", start100ns)
	}
}

// TestFlattenChaptersEmptyTitleFilter kills the Title != ""
// CONDITIONALS_NEGATION mutant.  Untitled entries must be dropped.
func TestFlattenChaptersEmptyTitleFilter(t *testing.T) {
	in := []mkv.Chapter{
		{StartMs: 0, Title: "Intro"},
		{StartMs: 1000, Title: ""},
		{StartMs: 2000, Title: "End"},
	}
	out := flattenChapters(in)
	if len(out) != 2 || out[0].Title != "Intro" || out[1].Title != "End" {
		t.Errorf("flattenChapters = %v, want [Intro, End]", out)
	}
}

// ── moov.go: mdhdLanguage boundary ─────────────────────────────────────────

// TestMdhdLanguageBoundary kills the len(t.Language) == 3
// CONDITIONALS_NEGATION mutant.  Exactly 3 chars must pass through; anything
// else must return "und".
func TestMdhdLanguageBoundary(t *testing.T) {
	if got := mdhdLanguage(mkv.Track{Language: "fre"}); got != "fre" {
		t.Errorf("3-char: %q, want fre", got)
	}
	if got := mdhdLanguage(mkv.Track{Language: "fr"}); got != "und" {
		t.Errorf("2-char: %q, want und", got)
	}
	if got := mdhdLanguage(mkv.Track{Language: "fren"}); got != "und" {
		t.Errorf("4-char: %q, want und", got)
	}
	if got := mdhdLanguage(mkv.Track{}); got != "und" {
		t.Errorf("empty: %q, want und", got)
	}
}

// ── codec.go: paspBox num==den guard ───────────────────────────────────────

// TestPaspBoxSquarePixels kills the num == den CONDITIONALS_NEGATION mutant.
// Square pixels (dw*h == dh*w) must return nil; non-square must not.
func TestPaspBoxSquarePixels(t *testing.T) {
	w := uint32(1920); h := uint32(1080)
	dw := uint32(1920); dh := uint32(1080)
	tr := &mkv.Track{Width: &w, Height: &h, DisplayWidth: &dw, DisplayHeight: &dh}
	if paspBox(tr) != nil {
		t.Error("square pixels (1920×1080 = 1920×1080) must return nil pasp")
	}
	// Non-square: DisplayWidth*Height ≠ DisplayHeight*Width.
	dw2 := uint32(1024); dh2 := uint32(576) // 16:9 display on 720×576 coded
	w2 := uint32(720); h2 := uint32(576)
	tr2 := &mkv.Track{Width: &w2, Height: &h2, DisplayWidth: &dw2, DisplayHeight: &dh2}
	if paspBox(tr2) == nil {
		t.Error("anamorphic pixels must return non-nil pasp")
	}
}

// TestGCDU64Arithmetic kills ARITHMETIC_BASE mutants on the gcd loop (a%b,
// etc.) and the a==0 safety guard.
func TestGCDU64Arithmetic(t *testing.T) {
	if got := gcdU64(12, 8); got != 4 {
		t.Errorf("gcd(12,8) = %d, want 4", got)
	}
	if got := gcdU64(15, 5); got != 5 {
		t.Errorf("gcd(15,5) = %d, want 5", got)
	}
	if got := gcdU64(7, 1); got != 1 {
		t.Errorf("gcd(7,1) = %d, want 1", got)
	}
	// gcd(n, 0): b=0, loop skips, returns a.
	if got := gcdU64(5, 0); got != 5 {
		t.Errorf("gcd(5,0) = %d, want 5", got)
	}
	// gcd(0, 0): a=0 → safety guard returns 1.
	if got := gcdU64(0, 0); got != 1 {
		t.Errorf("gcd(0,0) = %d, want 1", got)
	}
	if got := gcdU64(17, 13); got != 1 {
		t.Errorf("gcd(17,13) = %d, want 1 (coprime)", got)
	}
}

// TestAudioSampleEntryDefaults exercises the channels>0 and sampleRate>0
// guard paths in audioSampleEntry. When both are nil the defaults (2ch, 48000
// Hz) are used; when provided they override.
func TestAudioSampleEntryDefaults(t *testing.T) {
	// No channels/rate → defaults: 2 ch, 48000 Hz.
	tr := &mkv.Track{ID: 1, Codec: "aac", CodecPrivate: []byte{0x12, 0x10}}
	entry, err := aacEntry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	// boxf header(8) + reserved(6) + dataRefIdx(2) + reserved(8) = 24 → channels.
	ch := binary.BigEndian.Uint16(entry[24:26])
	if ch != 2 {
		t.Errorf("default channels = %d, want 2", ch)
	}
	// 24 + channels(2) + samplesize(2) + pre_defined(2) + reserved(2) = 32 → samplerate.
	const wantRate = uint32(48000) << 16 // 16.16 fixed-point
	rateFixed := binary.BigEndian.Uint32(entry[32:36])
	if rateFixed != wantRate {
		t.Errorf("default rate fixed-point = %#x, want %#x", rateFixed, wantRate)
	}
}

// ── box.go: isLowerAlpha boundaries ────────────────────────────────────────

// TestIsLowerAlphaBoundaries kills the s[i] < 'a' and s[i] > 'z'
// CONDITIONALS_BOUNDARY mutants by testing characters at exactly ±1 of the
// boundary.
func TestIsLowerAlphaBoundaries(t *testing.T) {
	// '`' is 'a'-1: must NOT be lower alpha.
	if isLowerAlpha("`bc") {
		t.Error("`bc: '`' ('a'-1) is not lower alpha")
	}
	// '{' is 'z'+1: must NOT be lower alpha.
	if isLowerAlpha("az{") {
		t.Error("az{: '{' ('z'+1) is not lower alpha")
	}
	// Exactly 'a' and 'z' must be valid.
	if !isLowerAlpha("az") {
		t.Error("az: both boundary letters must be valid")
	}
	// Upper-case letter must fail.
	if isLowerAlpha("Abc") {
		t.Error("Abc: upper case must not be lower alpha")
	}
	// Empty string: vacuously true (no character to reject).
	if !isLowerAlpha("") {
		t.Error("empty string must be lower alpha")
	}
}

// ── webvtt.go: wvttEntry CodecPrivate paths ─────────────────────────────────

// TestWVTTEntryCodecPrivateBranches covers the len(CodecPrivate)>0 and
// strings.HasPrefix checks in wvttEntry, killing the CONDITIONALS_NEGATION
// mutants on both.
func TestWVTTEntryCodecPrivateBranches(t *testing.T) {
	// No CodecPrivate → bare "WEBVTT".
	e1, err := wvttEntry(&mkv.Track{ID: 1, Codec: "webvtt"}, nil)
	if err != nil {
		t.Fatalf("no CodecPrivate: %v", err)
	}
	if !bytes.Contains(e1, []byte("WEBVTT")) {
		t.Error("no-CP entry must contain WEBVTT")
	}

	// CodecPrivate starts with "WEBVTT" → used verbatim (not wrapped again).
	cp := []byte("WEBVTT\nNOTE my header")
	e2, _ := wvttEntry(&mkv.Track{ID: 1, Codec: "webvtt", CodecPrivate: cp}, nil)
	if !bytes.Contains(e2, cp) {
		t.Errorf("verbatim CP not preserved")
	}

	// CodecPrivate does NOT start with "WEBVTT" → prepend "WEBVTT\n".
	cp2 := []byte("NOTE no header")
	e3, _ := wvttEntry(&mkv.Track{ID: 1, Codec: "webvtt", CodecPrivate: cp2}, nil)
	if !bytes.Contains(e3, []byte("WEBVTT\nNOTE no header")) {
		t.Errorf("non-WEBVTT CP not prefixed: %q", e3)
	}
}

// TestDecodeWVTTMultipleCues verifies that multiple vttc boxes in one sample
// are concatenated with a newline, and that non-vttc boxes are ignored.
func TestDecodeWVTTMultipleCues(t *testing.T) {
	payl1 := boxf("payl", func(w *bw) { w.bytes([]byte("First")) })
	payl2 := boxf("payl", func(w *bw) { w.bytes([]byte("Second")) })
	vttc1 := boxf("vttc", func(w *bw) { w.bytes(payl1) })
	vttc2 := boxf("vttc", func(w *bw) { w.bytes(payl2) })
	other := box("vtte", nil) // filler, must be ignored
	sample := append(append(append([]byte{}, other...), vttc1...), vttc2...)

	got, ok := decodeWVTT(sample)
	if !ok {
		t.Fatal("decodeWVTT: want ok=true")
	}
	want := []byte("First\nSecond")
	if !bytes.Equal(got, want) {
		t.Errorf("decodeWVTT = %q, want %q", got, want)
	}
}

// ── webvtt.go: extractWVTTConfig boundary ──────────────────────────────────

// TestExtractWVTTConfigPlainTextHdrBoundary kills the
// len(payload) < plainTextHdr CONDITIONALS_BOUNDARY mutant.
// Exactly 8 bytes (== plainTextHdr) followed by a vttC must set CodecPrivate;
// 7 bytes must leave it nil.
func TestExtractWVTTConfigPlainTextHdrBoundary(t *testing.T) {
	// 7-byte payload → return early, no CodecPrivate.
	var tr inTrack
	extractWVTTConfig(&tr, make([]byte, 7))
	if tr.codecPrivate != nil {
		t.Error("7-byte payload must not set CodecPrivate")
	}

	// 8-byte header + vttC → must set CodecPrivate to "WEBVTT".
	vttc := boxf("vttC", func(w *bw) { w.bytes([]byte("WEBVTT")) })
	payload := append(make([]byte, 8), vttc...)
	var tr2 inTrack
	extractWVTTConfig(&tr2, payload)
	if !bytes.Equal(tr2.codecPrivate, []byte("WEBVTT")) {
		t.Errorf("vttC config = %q, want WEBVTT", tr2.codecPrivate)
	}
}

// ── subtitle_webvtt.go: trackID=1 can be subtitle ──────────────────────────

// TestExtractSubtitleWebVTTTrackID1 kills the idx < 0 → idx <= 0
// CONDITIONALS_BOUNDARY mutant.  When the subtitle is mv.tracks[0]
// (trackID=1), the bounds check idx <= 0 would incorrectly return "not found".
func TestExtractSubtitleWebVTTTrackID1(t *testing.T) {
	// Put subtitle first, video second in the MKV.
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.SubtitleTrack, Codec: "srt"},
		{ID: 2, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 500, key: true, data: []byte("Hello")},
		{track: 2, pts: 0, key: true, data: []byte{1}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "sub1.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}

	// The subtitle is mv.tracks[0] → trackID=1 for ExtractSubtitleWebVTT.
	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 1, &b); err != nil {
		t.Fatalf("trackID=1 subtitle should work: %v", err)
	}
	if !strings.HasPrefix(b.String(), "WEBVTT") {
		t.Errorf("output not WebVTT: %q", b.String())
	}
}

// TestExtractSubtitleWebVTTOutOfRangeTrackID kills the idx >= len(mv.tracks)
// guard: an out-of-range trackID must error, not panic.
func TestExtractSubtitleWebVTTOutOfRangeTrackID(t *testing.T) {
	mp4Path := buildTestMP4(t) // 2 tracks (video + audio)
	var b strings.Builder
	// trackID=0 → idx=-1 → error.
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 0, &b); err == nil {
		t.Error("trackID=0 must error (idx<0)")
	}
	// trackID=100 → idx=99 → error (only 2 tracks).
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 100, &b); err == nil {
		t.Error("trackID=100 must error (out of range)")
	}
}
