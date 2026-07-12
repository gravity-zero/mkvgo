package mp4

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestParseElst(t *testing.T) {
	// v0, one non-empty edit: media_time 7507, no empty delay.
	var v0 bw
	v0.u8(0)
	v0.u24(0)          // flags
	v0.u32(1)          // entry_count
	v0.u32(1000)       // segment_duration
	v0.u32(7507)       // media_time
	v0.u32(0x00010000) // media_rate
	if mt, empty, ok := parseElst(v0.b); !ok || mt != 7507 || empty != 0 {
		t.Errorf("v0 = (%d, %d, %v), want (7507, 0, true)", mt, empty, ok)
	}

	// Leading empty edit (media_time -1, duration 100) then a non-empty edit.
	var mixed bw
	mixed.u8(0)
	mixed.u24(0)
	mixed.u32(2)
	mixed.u32(100)        // empty edit segment_duration (movie ticks)
	mixed.u32(0xFFFFFFFF) // media_time -1
	mixed.u32(0x00010000)
	mixed.u32(500)
	mixed.u32(50) // non-empty media_time
	mixed.u32(0x00010000)
	if mt, empty, ok := parseElst(mixed.b); !ok || mt != 50 || empty != 100 {
		t.Errorf("mixed = (%d, %d, %v), want (50, 100, true)", mt, empty, ok)
	}

	// v1 (64-bit) non-empty edit.
	var v1 bw
	v1.u8(1)
	v1.u24(0)
	v1.u32(1)
	v1.u64(1000) // segment_duration
	v1.u64(7507) // media_time
	v1.u32(0x00010000)
	if mt, empty, ok := parseElst(v1.b); !ok || mt != 7507 || empty != 0 {
		t.Errorf("v1 = (%d, %d, %v), want (7507, 0, true)", mt, empty, ok)
	}

	// No edit that shifts the timeline.
	if _, _, ok := parseElst([]byte{0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("empty edit list should report ok=false")
	}
}

// bytesU32 builds a box payload from a sequence of big-endian uint32 fields.
func bytesU32(vals ...uint32) []byte {
	var w bw
	for _, v := range vals {
		w.u32(v)
	}
	return w.b
}

// editStbl is a minimal sample table: 3 samples, 1s apart, composition offset 83
// ticks (so cts = 83/1083/2083 ms at timescale 1000).
func editStbl() []memBox {
	return []memBox{
		{typ: "stsz", payload: bytesU32(0, 0, 3, 10, 10, 10)},
		{typ: "stco", payload: bytesU32(0, 1, 0)},
		{typ: "stsc", payload: bytesU32(0, 1, 1, 3, 1)},
		{typ: "stts", payload: bytesU32(0, 1, 3, 1000)},
		{typ: "ctts", payload: bytesU32(0, 1, 3, 83)},
	}
}

// TestBuildSampleTableEditShift checks the edit-list shift is folded into the
// composition times - so both the remux and the keyframe index see it - and that a
// presentation time before the edit start is clamped to 0.
func TestBuildSampleTableEditShift(t *testing.T) {
	// media_time 83 ms trimmed: cts 83/1083/2083 → 0/1000/2000.
	tr := inTrack{timescale: 1000, editShiftMs: -83}
	if err := buildSampleTable(&tr, editStbl(), 1000); err != nil {
		t.Fatalf("buildSampleTable: %v", err)
	}
	want := []int64{0, 1000, 2000}
	for i, s := range tr.samples {
		if s.ctsMs != want[i] {
			t.Errorf("sample %d ctsMs = %d, want %d", i, s.ctsMs, want[i])
		}
	}
	// videoKeyframesMs reads the already-shifted times (every sample is sync here).
	mv := &movie{tracks: []inTrack{{trackType: mkv.VideoTrack, samples: tr.samples}}}
	for i, k := range videoKeyframesMs(mv) {
		if k != want[i] {
			t.Errorf("keyframe %d = %d, want %d", i, k, want[i])
		}
	}

	// A larger trim pushes the early cues before 0 → clamped, not negative.
	tr2 := inTrack{timescale: 1000, editShiftMs: -2000}
	if err := buildSampleTable(&tr2, editStbl(), 1000); err != nil {
		t.Fatalf("buildSampleTable: %v", err)
	}
	if tr2.samples[0].ctsMs != 0 || tr2.samples[2].ctsMs != 83 {
		t.Errorf("clamped cts = %d/%d/%d, want 0/0/83",
			tr2.samples[0].ctsMs, tr2.samples[1].ctsMs, tr2.samples[2].ctsMs)
	}
}

// TestChapterTrackRefs checks tref/chap references are collected so the chapter
// track is not surfaced as a dropped track.
func TestChapterTrackRefs(t *testing.T) {
	moovBoxes := []memBox{
		{typ: "trak", payload: box("tref", box("chap", bytesU32(3)))},
		{typ: "trak", payload: nil}, // a track without tref
	}
	ids := chapterTrackRefs(moovBoxes)
	if !ids[3] || len(ids) != 1 {
		t.Errorf("chapterTrackRefs = %v, want {3}", ids)
	}
}

// TestVideoFrameRate checks the average-frame-rate derivation from sample timing.
func TestVideoFrameRate(t *testing.T) {
	s40 := []inSample{{durMs: 40}, {durMs: 40}, {durMs: 40}} // 25 fps
	if fps := videoFrameRate(s40); fps != 25 {
		t.Errorf("videoFrameRate(40ms) = %v, want 25", fps)
	}
	if fps := videoFrameRate(nil); fps != 0 {
		t.Errorf("videoFrameRate(nil) = %v, want 0 (no samples)", fps)
	}
	if fps := videoFrameRate([]inSample{{durMs: 40}}); fps != 0 {
		t.Errorf("videoFrameRate(1 sample) = %v, want 0", fps)
	}
}
