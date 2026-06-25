package mp4

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// tfraPayload builds a version-0 tfra body (1-byte traf/trun/sample fields) for
// trackID with the given random-access times.
func tfraPayload(trackID uint32, times []uint32) []byte {
	p := []byte{0, 0, 0, 0} // version 0 + flags
	p = append(p, u32be(trackID)...)
	p = append(p, 0, 0, 0, 0) // reserved + length_size fields (0 → 1-byte each)
	p = append(p, u32be(uint32(len(times)))...)
	for _, tk := range times {
		p = append(p, u32be(tk)...) // time
		p = append(p, u32be(0)...)  // moof_offset
		p = append(p, 1, 1, 1)      // traf / trun / sample numbers
	}
	return p
}

func TestTrexDefaultDurations(t *testing.T) {
	// trex: version/flags, track_ID, default_sample_description_index,
	// default_sample_duration, default_sample_size, default_sample_flags.
	trex := box4("trex", bytes.Join([][]byte{{0, 0, 0, 0}, u32be(2), u32be(1), u32be(1500), u32be(0), u32be(0)}, nil))
	got := trexDefaultDurations([]memBox{{typ: "mvex", payload: trex}})
	if got[2] != 1500 {
		t.Errorf("default_sample_duration[2] = %d, want 1500", got[2])
	}
	if d := trexDefaultDurations(nil); d != nil {
		t.Error("no mvex must yield nil")
	}
}

func TestParseTfra(t *testing.T) {
	tid, got, ok := parseTfra(tfraPayload(7, []uint32{0, 1000, 2000, 3000}))
	if !ok || tid != 7 {
		t.Fatalf("parseTfra = trackID %d ok %v", tid, ok)
	}
	if want := []uint64{0, 1000, 2000, 3000}; !reflect.DeepEqual(got, want) {
		t.Errorf("times = %v, want %v", got, want)
	}
	if _, _, ok := parseTfra([]byte{0, 0, 0}); ok {
		t.Error("a truncated tfra must report not-ok")
	}
}

func TestReadFragmentKeyframes(t *testing.T) {
	tfra := box4("tfra", tfraPayload(1, []uint32{0, 500, 2000}))
	mfraSize := 8 + len(tfra) + 16
	mfro := box4("mfro", append([]byte{0, 0, 0, 0}, u32be(uint32(mfraSize))...))
	mfra := box4("mfra", append(tfra, mfro...))
	if len(mfra) != mfraSize {
		t.Fatalf("mfra size %d != computed %d", len(mfra), mfraSize)
	}
	file := append(make([]byte, 200), mfra...) // mfra at the file tail

	mv := &movie{tracks: []inTrack{{trackID: 1, trackType: mkv.VideoTrack, timescale: 1000}}}
	readFragmentKeyframes(bytes.NewReader(file), int64(len(file)), mv)
	if want := []int64{0, 500, 2000}; !reflect.DeepEqual(mv.tracks[0].keyframesMs, want) {
		t.Errorf("keyframes = %v, want %v", mv.tracks[0].keyframesMs, want)
	}

	// No mfro at the tail → keyframes left nil.
	mv2 := &movie{tracks: []inTrack{{trackID: 1, trackType: mkv.VideoTrack, timescale: 1000}}}
	readFragmentKeyframes(bytes.NewReader(make([]byte, 200)), 200, mv2)
	if mv2.tracks[0].keyframesMs != nil {
		t.Error("no mfra index → keyframes must stay nil")
	}
}
