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

// TestVideoKeyframesEditShift checks the edit-list shift is applied to keyframe
// timestamps and that pre-roll keyframes (negative after the shift) are dropped.
func TestVideoKeyframesEditShift(t *testing.T) {
	mv := &movie{tracks: []inTrack{{
		trackType:   mkv.VideoTrack,
		editShiftMs: -83, // media_time 83 ms trimmed from the start
		samples: []inSample{
			{ctsMs: 83, sync: true},
			{ctsMs: 1083, sync: true},
			{ctsMs: 2083, sync: true},
		},
	}}}
	got := videoKeyframesMs(mv)
	want := []int64{0, 1000, 2000}
	if len(got) != len(want) {
		t.Fatalf("keyframes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keyframes = %v, want %v", got, want)
		}
	}

	// A keyframe whose presentation time falls before the edit start is dropped.
	mv.tracks[0].samples = []inSample{
		{ctsMs: 0, sync: true},    // -83 after shift → dropped
		{ctsMs: 83, sync: true},   // 0
		{ctsMs: 1083, sync: true}, // 1000
	}
	got = videoKeyframesMs(mv)
	if len(got) != 2 || got[0] != 0 || got[1] != 1000 {
		t.Errorf("with pre-roll keyframes = %v, want [0 1000]", got)
	}
}
