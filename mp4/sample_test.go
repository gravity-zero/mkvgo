package mp4

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func samplesFromPTS(pts []int64, sync []bool) []sample {
	out := make([]sample, len(pts))
	for i := range pts {
		out[i] = sample{size: 10, pts: pts[i], sync: sync[i]}
	}
	return out
}

func TestReconstructTimingNoBFrames(t *testing.T) {
	s := samplesFromPTS([]int64{0, 40, 80, 120}, []bool{true, false, false, false})
	tim := reconstructTiming(s, 40)
	if !reflect.DeepEqual(tim.durations, []int64{40, 40, 40, 40}) {
		t.Errorf("durations = %v", tim.durations)
	}
	if tim.hasCTTS {
		t.Errorf("expected no ctts for monotonic PTS")
	}
	if tim.total != 160 {
		t.Errorf("total = %d, want 160", tim.total)
	}
}

func TestReconstructTimingBFrames(t *testing.T) {
	// Decode order with reordering: PTS go backwards (B-frames).
	s := samplesFromPTS([]int64{0, 120, 40, 80}, []bool{true, false, false, false})
	tim := reconstructTiming(s, 40)
	if !tim.hasCTTS {
		t.Fatalf("expected ctts for reordered PTS")
	}
	// DTS = sorted PTS = [0,40,80,120]; ctts = PTS - DTS.
	wantCTTS := []int32{0, 80, -40, -40}
	if !reflect.DeepEqual(tim.ctts, wantCTTS) {
		t.Errorf("ctts = %v, want %v", tim.ctts, wantCTTS)
	}
	// Durations come from the DTS deltas: all 40.
	if !reflect.DeepEqual(tim.durations, []int64{40, 40, 40, 40}) {
		t.Errorf("durations = %v", tim.durations)
	}
	if tim.total != 160 {
		t.Errorf("total = %d, want 160", tim.total)
	}
}

func TestReconstructTimingSingleSample(t *testing.T) {
	s := samplesFromPTS([]int64{0}, []bool{true})
	tim := reconstructTiming(s, 0)
	if len(tim.durations) != 1 || tim.durations[0] != 1 {
		t.Errorf("single-sample duration = %v, want [1]", tim.durations)
	}
}

func TestReconstructTimingLastDurFallback(t *testing.T) {
	// lastDurMs=0 and n>1 → reuse previous delta.
	s := samplesFromPTS([]int64{0, 30, 60}, []bool{true, false, false})
	tim := reconstructTiming(s, 0)
	if tim.durations[2] != 30 {
		t.Errorf("last duration = %d, want 30 (reused delta)", tim.durations[2])
	}
}

func TestBuildSTTSRunLength(t *testing.T) {
	b := buildSTTS([]int64{40, 40, 40, 33})
	// fullbox(12) + entry_count(4) + 2 runs * 8
	if string(b[4:8]) != "stts" {
		t.Fatalf("not stts")
	}
	entryCount := binary.BigEndian.Uint32(b[12:16])
	if entryCount != 2 {
		t.Fatalf("entry_count = %d, want 2", entryCount)
	}
	// run 1: count 3 delta 40
	if binary.BigEndian.Uint32(b[16:20]) != 3 || binary.BigEndian.Uint32(b[20:24]) != 40 {
		t.Errorf("run1 wrong: % x", b[16:24])
	}
	// run 2: count 1 delta 33
	if binary.BigEndian.Uint32(b[24:28]) != 1 || binary.BigEndian.Uint32(b[28:32]) != 33 {
		t.Errorf("run2 wrong: % x", b[24:32])
	}
}

func TestBuildSTSSOmittedWhenAllSync(t *testing.T) {
	all := samplesFromPTS([]int64{0, 1, 2}, []bool{true, true, true})
	if b := buildSTSS(all); b != nil {
		t.Errorf("stss should be nil when every sample is sync")
	}
	mixed := samplesFromPTS([]int64{0, 1, 2, 3}, []bool{true, false, false, true})
	b := buildSTSS(mixed)
	if b == nil {
		t.Fatal("stss should be present for mixed sync flags")
	}
	if binary.BigEndian.Uint32(b[12:16]) != 2 {
		t.Errorf("sync count = %d, want 2", binary.BigEndian.Uint32(b[12:16]))
	}
	// 1-based sample numbers: 1 and 4
	if binary.BigEndian.Uint32(b[16:20]) != 1 || binary.BigEndian.Uint32(b[20:24]) != 4 {
		t.Errorf("sync sample numbers wrong: % x", b[16:24])
	}
}

func TestBuildCTTSVersion1(t *testing.T) {
	tim := timing{ctts: []int32{0, -40, -40, 80}, hasCTTS: true}
	b := buildCTTS(tim)
	if b == nil {
		t.Fatal("ctts should be present")
	}
	if b[8] != 1 {
		t.Errorf("ctts version = %d, want 1 (signed offsets)", b[8])
	}
	// Runs: {1,0}, {2,-40}, {1,80} → entry_count 3.
	if ec := binary.BigEndian.Uint32(b[12:16]); ec != 3 {
		t.Errorf("entry_count = %d, want 3", ec)
	}
	// Run2 = {count:2, offset:-40} at entries offset 8..16 after entry_count.
	if cnt := binary.BigEndian.Uint32(b[24:28]); cnt != 2 {
		t.Errorf("second run count = %d, want 2", cnt)
	}
	if off := int32(binary.BigEndian.Uint32(b[28:32])); off != -40 {
		t.Errorf("second run offset = %d, want -40", off)
	}
}

func TestBuildCTTSNilWhenZero(t *testing.T) {
	if b := buildCTTS(timing{hasCTTS: false}); b != nil {
		t.Errorf("ctts must be nil when no offsets")
	}
}

func TestBuildSTSCRunLength(t *testing.T) {
	chunks := []chunk{{count: 5}, {count: 5}, {count: 3}}
	b := buildSTSC(chunks)
	// entry_count should be 2: chunks 1-2 have 5, chunk 3 has 3.
	if ec := binary.BigEndian.Uint32(b[12:16]); ec != 2 {
		t.Fatalf("entry_count = %d, want 2", ec)
	}
	// entry1: first_chunk 1, spc 5, desc 1
	if binary.BigEndian.Uint32(b[16:20]) != 1 || binary.BigEndian.Uint32(b[20:24]) != 5 {
		t.Errorf("entry1 wrong")
	}
	// entry2: first_chunk 3, spc 3
	if binary.BigEndian.Uint32(b[28:32]) != 3 || binary.BigEndian.Uint32(b[32:36]) != 3 {
		t.Errorf("entry2 wrong")
	}
}

func TestBuildChunkOffsets(t *testing.T) {
	// 32-bit (stco), with a base added to the relative offsets.
	small := buildChunkOffsets([]chunk{{offset: 100}, {offset: 200}}, 1000, false)
	if string(small[4:8]) != "stco" {
		t.Fatalf("expected stco, got %q", small[4:8])
	}
	if binary.BigEndian.Uint32(small[16:20]) != 1100 || binary.BigEndian.Uint32(small[20:24]) != 1200 {
		t.Errorf("stco offsets = %d,%d, want 1100,1200",
			binary.BigEndian.Uint32(small[16:20]), binary.BigEndian.Uint32(small[20:24]))
	}
	// 64-bit (co64).
	big := buildChunkOffsets([]chunk{{offset: 0x1_0000_0000}}, 0, true)
	if string(big[4:8]) != "co64" {
		t.Errorf("expected co64, got %q", big[4:8])
	}
	if binary.BigEndian.Uint64(big[16:24]) != 0x1_0000_0000 {
		t.Errorf("co64 offset wrong")
	}
}

func TestNeedCo64(t *testing.T) {
	if needCo64(1 << 20) {
		t.Error("small payload should not need co64")
	}
	if !needCo64(0xFFFFFFFF) {
		t.Error("near-4GiB payload should need co64")
	}
}

func TestBuildSTSZ(t *testing.T) {
	s := []sample{{size: 11}, {size: 22}, {size: 33}}
	b := buildSTSZ(s)
	if binary.BigEndian.Uint32(b[12:16]) != 0 {
		t.Errorf("sample_size field should be 0 (non-uniform)")
	}
	if binary.BigEndian.Uint32(b[16:20]) != 3 {
		t.Errorf("sample_count = %d, want 3", binary.BigEndian.Uint32(b[16:20]))
	}
	if binary.BigEndian.Uint32(b[20:24]) != 11 || binary.BigEndian.Uint32(b[28:32]) != 33 {
		t.Errorf("sizes wrong: % x", b[20:32])
	}
}
