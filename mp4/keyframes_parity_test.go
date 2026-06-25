package mp4

import (
	"io"
	"os"
	"reflect"
	"testing"
)

// TestKeyframesOnlyMatchesFullTable locks the core contract of the cheap
// keyframe path: parsing in sampleKeyframes mode (stss + stts/ctts, no offsets)
// yields the exact same keyframe timestamps as the full sample table.
func TestKeyframesOnlyMatchesFullTable(t *testing.T) {
	path := buildTestMP4(t)
	full := keyframesViaMode(t, path, sampleFull)
	cheap := keyframesViaMode(t, path, sampleKeyframes)

	if len(cheap) == 0 {
		t.Fatal("fixture produced no keyframes — test would be vacuous")
	}
	if !reflect.DeepEqual(cheap, full) {
		t.Errorf("keyframes-only = %v\n        full table = %v", cheap, full)
	}
}

func keyframesViaMode(t *testing.T, path string, mode sampleMode) []int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	mv, err := parseMP4(f, size, mode)
	if err != nil {
		t.Fatalf("parseMP4(%v): %v", mode, err)
	}
	return videoKeyframesMs(mv)
}
