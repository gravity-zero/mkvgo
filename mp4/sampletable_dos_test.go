package mp4

import (
	"encoding/binary"
	"testing"
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
