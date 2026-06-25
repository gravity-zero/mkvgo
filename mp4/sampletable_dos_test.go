package mp4

import (
	"encoding/binary"
	"testing"
	"time"
)

// TestStszComplexityGuard covers the constant-size stsz DoS: a tiny box must not
// be trusted to declare a sample count the file cannot physically hold (which
// maxSamples alone, 134M, does not stop), so no giant allocation/loop happens.
func TestStszComplexityGuard(t *testing.T) {
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:8], 4)           // sample_size = 4 (constant)
	binary.BigEndian.PutUint32(stsz[8:12], maxSamples) // 134M samples → 536MB if trusted

	if _, err := parseStsz(stsz, 1000); err == nil {
		t.Error("constant-size stsz far exceeding the file size must be rejected")
	}

	binary.BigEndian.PutUint32(stsz[8:12], 10) // 10 × 4 = 40 ≤ 1000
	if sizes, err := parseStsz(stsz, 1000); err != nil || len(sizes) != 10 {
		t.Errorf("valid constant-size stsz: len=%d err=%v, want 10/nil", len(sizes), err)
	}

	binary.BigEndian.PutUint32(stsz[8:12], 1000) // fileSize 0 disables the bound
	if sizes, err := parseStsz(stsz, 0); err != nil || len(sizes) != 1000 {
		t.Errorf("fileSize 0 should disable the bound: len=%d err=%v", len(sizes), err)
	}
}

// TestBuildSampleTableLinear covers the chunk/stsc complexity DoS: a forged file
// with N chunks AND N stsc entries used to be O(N²) (samplesForChunk scanned all
// entries per chunk), a multi-second stall on a small file. The monotonic cursor
// makes it linear, so even N=100k completes near-instantly.
func TestBuildSampleTableLinear(t *testing.T) {
	const N = 100_000

	stco := make([]byte, 8+N*4)
	binary.BigEndian.PutUint32(stco[4:], N)
	for i := 0; i < N; i++ {
		binary.BigEndian.PutUint32(stco[8+i*4:], uint32(i)) // chunk i at offset i
	}
	stsc := make([]byte, 8+N*12)
	binary.BigEndian.PutUint32(stsc[4:], N)
	for i := 0; i < N; i++ {
		b := 8 + i*12
		binary.BigEndian.PutUint32(stsc[b:], uint32(i+1)) // firstChunk
		binary.BigEndian.PutUint32(stsc[b+4:], 1)         // samples per chunk
		binary.BigEndian.PutUint32(stsc[b+8:], 1)         // sample description index
	}
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[4:], 1) // constant sample size 1
	binary.BigEndian.PutUint32(stsz[8:], N) // sample count
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:], 1) // one entry
	binary.BigEndian.PutUint32(stts[8:], N) // covering N samples (delta 0)

	boxes := []memBox{
		{typ: "stsz", payload: stsz},
		{typ: "stco", payload: stco},
		{typ: "stsc", payload: stsc},
		{typ: "stts", payload: stts},
	}

	var tr inTrack
	start := time.Now()
	if err := buildSampleTable(&tr, boxes, N); err != nil {
		t.Fatalf("buildSampleTable: %v", err)
	}
	if len(tr.samples) != N {
		t.Fatalf("samples = %d, want %d", len(tr.samples), N)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("buildSampleTable took %v for %d chunks×entries — quadratic regression", d, N)
	}
}

// TestKeyframeTimesComplexityGuard covers the same DoS on the keyframe-index path:
// headerFrameCount feeds n-sized allocations and an O(n) loop, so a 134M count on
// a tiny file must be rejected before any of that.
func TestKeyframeTimesComplexityGuard(t *testing.T) {
	stsz := make([]byte, 12)
	binary.BigEndian.PutUint32(stsz[8:12], maxSamples)
	boxes := []memBox{{typ: "stsz", payload: stsz}}

	if _, _, err := buildKeyframeTimes(boxes, 1000, 0, true, 1000); err == nil {
		t.Error("buildKeyframeTimes must reject a sample count larger than the file")
	}
}
