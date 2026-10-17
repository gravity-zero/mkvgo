package mp4

import "testing"

// TestNEW07_VP9ProtectedBlockAligned: the VP codec binding requires each frame's encrypted bytes block-aligned, the clear bytes before them taking the remainder.
func TestNEW07_VP9ProtectedBlockAligned(t *testing.T) {
	frame1, frame2 := vp9SyntheticFrame(20, 0x10), vp9SyntheticFrame(30, 0x40)
	index := []byte{0xC1, byte(len(frame1)), byte(len(frame2)), 0xC1}
	samples := [][]byte{vp9SyntheticFrame(40, 0x80), vp9SyntheticFrame(5, 0x80), append(append(append([]byte{}, frame1...), frame2...), index...)}
	for i, sample := range samples {
		subs, err := splitVP9Subsamples(sample)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, s := range subs {
			if s.protected%16 != 0 {
				t.Errorf("sample %d: %d protected bytes, not a whole number of AES blocks (%+v)", i, s.protected, subs)
			}
			total += int(s.clear) + int(s.protected)
		}
		if total != len(sample) {
			t.Fatalf("sample %d: subsamples cover %d of %d bytes", i, total, len(sample))
		}
	}
}
