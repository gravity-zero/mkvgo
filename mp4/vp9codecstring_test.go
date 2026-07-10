package mp4

import "testing"

func TestVP9Level(t *testing.T) {
	cases := []struct {
		w, h uint32
		want byte
	}{
		{176, 144, 10},   // 25344 <= 36864
		{640, 360, 21},   // 230400 <= 245760
		{1280, 720, 31},  // 921600 <= 983040
		{1920, 1080, 40}, // 2073600 <= 2228224
		{3840, 2160, 50}, // 8294400 <= 8912896
	}
	for _, c := range cases {
		if got := vp9Level(u32p(c.w), u32p(c.h)); got != c.want {
			t.Errorf("vp9Level(%dx%d) = %d, want %d", c.w, c.h, got, c.want)
		}
	}
	if got := vp9Level(nil, nil); got != 10 {
		t.Errorf("vp9Level(nil) = %d, want 10", got)
	}
}

func TestVP9CodecStringFromSampleEntry(t *testing.T) {
	// A vp09 sample entry whose vpcC was derived from the first frame (no
	// CodecPrivate) must still yield a valid vp09.PP.LL.DD codec string with a
	// non-zero level - the case that made a real VP9 stream unplayable.
	rec := []byte{0, 21, 0x80, 1, 1, 1, 0, 0} // profile 0, level 2.1, bitdepth 8
	entry := visualSampleEntryForTest("vp09", fullBox("vpcC", 1, 0, func(w *bw) { w.bytes(rec) }))
	got := vp9RecordFromSampleEntry(entry)
	if len(got) < 3 || got[0] != 0 || got[1] != 21 {
		t.Fatalf("vp9RecordFromSampleEntry did not recover the record: %v", got)
	}
}

// visualSampleEntryForTest wraps a config box in a minimal vp09-tagged box so
// vp9RecordFromSampleEntry can locate the vpcC child.
func visualSampleEntryForTest(fourcc string, config []byte) []byte {
	return box(fourcc, append(make([]byte, 78), config...))
}
