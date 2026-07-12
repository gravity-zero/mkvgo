package mp4

import (
	"encoding/binary"
	"testing"
)

// sampletable_mut_test.go kills mutation-testing survivors in sampletable.go:
// boundary/arithmetic on the sample-count and chunk/stsc bookkeeping that
// controls real parsed values (sample counts, offsets, durations) or the
// accept/reject decision on malformed input.

// TestBuildKeyframeTimesSampleCountBoundary kills the CONDITIONALS_BOUNDARY on
// `fc > maxSamples` (sampletable.go:35): exactly maxSamples must be accepted,
// one more must be rejected. Both cases return before any stts-sized
// allocation happens (no stts box here), so the boundary is exercised cheaply.
func TestBuildKeyframeTimesSampleCountBoundary(t *testing.T) {
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[8:12], maxSamples)
	boxes := []memBox{{typ: "stsz", payload: stsz}}
	if _, _, err := buildKeyframeTimes(boxes, 1000, 0, true, maxSamples); err != nil {
		t.Errorf("fc == maxSamples must be accepted, got %v", err)
	}

	binary.BigEndian.PutUint32(stsz[8:12], maxSamples+1)
	if _, _, err := buildKeyframeTimes(boxes, 1000, 0, true, maxSamples+1); err == nil {
		t.Error("fc == maxSamples+1 must be rejected")
	}
}

// TestBuildKeyframeTimesFileSizeBoundary kills the CONDITIONALS_BOUNDARY on
// `fileSize > 0` and `fc > fileSize` (sampletable.go:38): fc == fileSize must
// pass, fc == fileSize+1 must be rejected, and fileSize == 0 must disable the
// bound entirely (even for a large fc).
func TestBuildKeyframeTimesFileSizeBoundary(t *testing.T) {
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[8:12], 1000)
	boxes := []memBox{{typ: "stsz", payload: stsz}}

	if _, _, err := buildKeyframeTimes(boxes, 1000, 0, true, 1000); err != nil {
		t.Errorf("fc == fileSize must be accepted, got %v", err)
	}
	if _, _, err := buildKeyframeTimes(boxes, 1000, 0, true, 999); err == nil {
		t.Error("fc == fileSize+1 must be rejected")
	}
	if _, _, err := buildKeyframeTimes(boxes, 1000, 0, true, 0); err != nil {
		t.Errorf("fileSize == 0 must disable the bound, got %v", err)
	}
}

// TestBuildKeyframeTimesCtsArithmetic kills the ARITHMETIC_BASE mutants on
// `dts+int64(ctts[i])` and `+ editShiftMs` (sampletable.go:59) by asserting the
// exact composition times a known stts/ctts/editShiftMs combination produces.
func TestBuildKeyframeTimesCtsArithmetic(t *testing.T) {
	boxes := []memBox{
		{typ: "stsz", payload: bytesU32(0, 0, 2)},
		{typ: "stts", payload: bytesU32(0, 1, 2, 40)},                // 2 samples, delta 40
		{typ: "ctts", payload: bytesU32(0, 2, 1, 10, 1, 0xFFFFFFFB)}, // +10, -5
	}
	times, _, err := buildKeyframeTimes(boxes, 1000, 100, true, 1000)
	if err != nil {
		t.Fatalf("buildKeyframeTimes: %v", err)
	}
	// i=0: dts=0, cts=(0+10)+100=110. i=1: dts=40, cts=(40-5)+100=135.
	want := []int64{110, 135}
	if len(times) != len(want) || times[0] != want[0] || times[1] != want[1] {
		t.Errorf("times = %v, want %v", times, want)
	}
}

// TestBuildSampleTableInnerChunkCapped kills the CONDITIONALS_BOUNDARY on
// `si < n` (sampletable.go:139): a chunk declaring more samples-per-chunk than
// the stsz actually lists must stop at n, never indexing sizes[n] (which would
// panic).
func TestBuildSampleTableInnerChunkCapped(t *testing.T) {
	boxes := []memBox{
		{typ: "stsz", payload: bytesU32(0, 0, 3, 10, 10, 10)}, // n=3 samples
		{typ: "stco", payload: bytesU32(0, 1, 0)},             // 1 chunk at offset 0
		{typ: "stsc", payload: bytesU32(0, 1, 1, 5, 1)},       // claims 5 samples/chunk
		{typ: "stts", payload: bytesU32(0, 1, 3, 1000)},
	}
	var tr inTrack
	tr.timescale = 1000
	if err := buildSampleTable(&tr, boxes, 1000); err != nil {
		t.Fatalf("buildSampleTable: %v", err)
	}
	if len(tr.samples) != 3 {
		t.Errorf("samples = %d, want 3 (capped at stsz's count, no panic)", len(tr.samples))
	}
}

// TestTicksToNsArithmetic kills the ARITHMETIC_BASE mutants on the
// round-to-nearest formula in ticksToNs (sampletable.go:186): 3 ticks at a
// timescale of 4 is exactly 0.75s = 750,000,000ns.
func TestTicksToNsArithmetic(t *testing.T) {
	if got := ticksToNs(3, 4); got != 750_000_000 {
		t.Errorf("ticksToNs(3, 4) = %d, want 750000000", got)
	}
}

// TestParseStszConstantSizeOverflowBoundary kills the ARITHMETIC_BASE on
// `int64(count)*int64(sampleSize)` (sampletable.go:218): a constant-size table
// whose declared byte total exactly equals the file size must be accepted;
// one byte more must be rejected.
func TestParseStszConstantSizeOverflowBoundary(t *testing.T) {
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:8], 100) // sample_size
	binary.BigEndian.PutUint32(stsz[8:12], 10) // count -> 1000 bytes total

	if _, err := parseStsz(stsz, 1000); err != nil {
		t.Errorf("count*size == fileSize must be accepted, got %v", err)
	}
	if _, err := parseStsz(stsz, 999); err == nil {
		t.Error("count*size == fileSize+1 must be rejected")
	}
}

// TestParseChunkOffsetsCo64Sequence kills the INCREMENT_DECREMENT on the co64
// loop counter and the ARITHMETIC_BASE on its byte-range computation
// (sampletable.go:261-262): with 2 entries, every offset must be read from its
// own 8-byte slot, in order.
func TestParseChunkOffsetsCo64Sequence(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(2)
	w.u64(0x1000)
	w.u64(0x2000)
	off, err := parseChunkOffsets([]memBox{{typ: "co64", payload: w.b}})
	if err != nil {
		t.Fatalf("parseChunkOffsets: %v", err)
	}
	if len(off) != 2 || off[0] != 0x1000 || off[1] != 0x2000 {
		t.Errorf("co64 offsets = %v, want [4096 8192]", off)
	}
}

// TestParseChunkOffsetsCo64ExactBoundary kills the CONDITIONALS_BOUNDARY on
// `len(co64.payload) < 8` (sampletable.go:253): a co64 box carrying exactly
// the 8-byte header (version/flags + a zero entry_count) is a valid, empty
// table - not an error - while one byte short is rejected.
func TestParseChunkOffsetsCo64ExactBoundary(t *testing.T) {
	empty := bytesU32(0, 0) // version/flags=0, count=0, exactly 8 bytes
	off, err := parseChunkOffsets([]memBox{{typ: "co64", payload: empty}})
	if err != nil || len(off) != 0 {
		t.Errorf("exactly-8-byte co64 (count 0) = %v, %v, want ([], nil)", off, err)
	}
	if _, err := parseChunkOffsets([]memBox{{typ: "co64", payload: empty[:7]}}); err == nil {
		t.Error("a 7-byte co64 must be rejected as too short")
	}
}

// TestParseCttsTruncatedCountRejected kills the ARITHMETIC_BASE on
// `8+int(count)*8` (sampletable.go:340): a ctts declaring more entries than it
// actually carries must be rejected (all-zero offsets), not read out of
// bounds. A well-formed single-entry ctts of the matching size must parse.
func TestParseCttsTruncatedCountRejected(t *testing.T) {
	valid := []memBox{{typ: "ctts", payload: bytesU32(0, 1, 2, 40)}} // 1 entry: run=2, offset=40
	offs := parseCtts(valid, 2)
	if offs[0] != 40 || offs[1] != 40 {
		t.Errorf("valid ctts offsets = %v, want [40 40]", offs)
	}

	// Declares 5 entries but the payload only has room for 1: must reject
	// (zero offsets), never read past the buffer.
	short := []memBox{{typ: "ctts", payload: bytesU32(0, 5, 2, 40)}}
	offs2 := parseCtts(short, 2)
	if offs2[0] != 0 || offs2[1] != 0 {
		t.Errorf("truncated-count ctts offsets = %v, want [0 0] (rejected)", offs2)
	}
}

// TestParseCttsIdxCapped kills the CONDITIONALS_BOUNDARY on `idx < n`
// (sampletable.go:344): a run longer than n must stop filling at n, never
// writing offsets[n] (which would panic).
func TestParseCttsIdxCapped(t *testing.T) {
	boxes := []memBox{{typ: "ctts", payload: bytesU32(0, 1, 5, 7)}} // run=5, only n=2 slots
	offs := parseCtts(boxes, 2)
	if len(offs) != 2 || offs[0] != 7 || offs[1] != 7 {
		t.Errorf("capped ctts offsets = %v, want [7 7], len 2", offs)
	}
}

// TestParseStssEmptyBoxIsNotAbsent kills the CONDITIONALS_BOUNDARY on
// `len(stss.payload) < 8` (sampletable.go:360): an stss box present with a
// declared entry_count of 0 means "no sample is a sync sample" (a non-nil,
// empty set) - semantically different from an ABSENT stss (nil, meaning every
// sample is sync). The exact-8-byte box must reach that empty-set path, not be
// rejected as if it were missing.
func TestParseStssEmptyBoxIsNotAbsent(t *testing.T) {
	boxes := []memBox{{typ: "stss", payload: bytesU32(0, 0)}} // version/flags=0, count=0
	got := parseStss(boxes)
	if got == nil {
		t.Fatal("an empty (but present) stss must return a non-nil empty set, not nil")
	}
	if len(got) != 0 {
		t.Errorf("empty stss set = %v, want empty", got)
	}

	// One byte short: genuinely too short, must fall back to nil (absent).
	shortBoxes := []memBox{{typ: "stss", payload: bytesU32(0, 0)[:7]}}
	if got := parseStss(shortBoxes); got != nil {
		t.Errorf("truncated stss = %v, want nil", got)
	}
}
