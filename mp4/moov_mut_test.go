package mp4

import (
	"encoding/binary"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// moov_mut_test.go kills mutation-testing survivors in moov.go: the edit-list
// arithmetic, the udta emission guard, and the audio-track media-timescale
// scaling - checked by inspecting the built boxes' actual bytes/fields.

// TestBuildMoovNoUdtaWhenEmpty kills the CONDITIONALS_BOUNDARY on
// `len(udtaKids) > 0` (moov.go:79): with no title/tags/cover/chapters, no udta
// box must be emitted at all (an empty udta box is a real, observable
// difference - unlike a for-loop over an empty slice, `container("udta")`
// still produces a real box).
func TestBuildMoovNoUdtaWhenEmpty(t *testing.T) {
	tr := makeOutTrackForMoov(1, 100)
	moov := buildMoov([]*outTrack{tr}, 0, false, movieMeta{})
	boxes, err := iterBoxes(moov[8:])
	if err != nil {
		t.Fatalf("iterBoxes(moov): %v", err)
	}
	if _, ok := findMemBox(boxes, "udta"); ok {
		t.Error("moov-level udta must be absent when there is no title/tags/cover/chapters/hashes")
	}
}

// TestBuildEdtsArithmeticAndBoundaries kills the moov.go:288-289 mutants
// (segDur arithmetic, its <0 negation-to->=0 clamp inversion) and the
// moov.go:297 CONDITIONALS_BOUNDARY on `offsetMovieMs > 0` (whether the
// leading empty edit is emitted at all), by decoding the built elst box's
// exact fields.
func TestBuildEdtsArithmeticAndBoundaries(t *testing.T) {
	// codecDelayNs = 40ms at mts=48000 -> exactly 1920 samples (clean, no
	// rounding ambiguity). durMovieMs=960, offsetMovieMs=250.
	edts := buildEdts(40_000_000, 250, 960, 48000)
	boxes, err := iterBoxes(edts[8:])
	if err != nil {
		t.Fatalf("iterBoxes(edts): %v", err)
	}
	elst, ok := findMemBox(boxes, "elst")
	if !ok {
		t.Fatal("elst box missing")
	}
	p := elst.payload
	count := binary.BigEndian.Uint32(p[4:8])
	if count != 2 {
		t.Fatalf("elst entry_count = %d, want 2 (offsetMovieMs > 0 must add the leading empty edit)", count)
	}
	segDur0 := binary.BigEndian.Uint32(p[8:12])
	mt0 := int32(binary.BigEndian.Uint32(p[12:16]))
	if segDur0 != 250 || mt0 != -1 {
		t.Errorf("empty edit = (segDur %d, mediaTime %d), want (250, -1)", segDur0, mt0)
	}
	segDur1 := binary.BigEndian.Uint32(p[20:24])
	mt1 := int32(binary.BigEndian.Uint32(p[24:28]))
	// segDur = 960 - 40 = 920 (must NOT be clamped to 0: proves the <0 check
	// isn't inverted to fire on a normal positive value).
	if segDur1 != 920 {
		t.Errorf("media edit segDur = %d, want 920 (960 - 40, not clamped)", segDur1)
	}
	if mt1 != 1920 {
		t.Errorf("media edit mediaTime = %d, want 1920 (40ms @ 48kHz)", mt1)
	}
}

// TestBuildEdtsNoOffsetOmitsEmptyEdit kills the other direction of the
// moov.go:297 boundary: offsetMovieMs == 0 must NOT add a leading empty edit.
func TestBuildEdtsNoOffsetOmitsEmptyEdit(t *testing.T) {
	edts := buildEdts(0, 0, 500, 1000)
	boxes, _ := iterBoxes(edts[8:])
	elst, ok := findMemBox(boxes, "elst")
	if !ok {
		t.Fatal("elst box missing")
	}
	if count := binary.BigEndian.Uint32(elst.payload[4:8]); count != 1 {
		t.Errorf("elst entry_count = %d, want 1 (no empty edit when offsetMovieMs == 0)", count)
	}
}

// makeAudioOutTrackForMoov builds an audio outTrack whose media timescale is
// its sample rate (mediaTimescale's "soun" branch), with samples inserted
// out of pts order so the offset-scan must actually chase the minimum rather
// than assume samples[0] is smallest.
func makeAudioOutTrackForMoov(mp4ID uint32, sampleRate float64, ptsMsInInsertOrder []int64) *outTrack {
	sr := sampleRate
	t := &outTrack{
		mp4ID:       mp4ID,
		mkv:         mkv.Track{ID: uint64(mp4ID), Type: mkv.AudioTrack, SampleRate: &sr},
		spec:        codecSpec{handler: "soun"},
		sampleEntry: make([]byte, 8),
	}
	for _, p := range ptsMsInInsertOrder {
		t.samples.addDur(10, p, p, 0, true)
	}
	t.samples.addChunk(0, len(ptsMsInInsertOrder))
	return t
}

// TestBuildTrakAudioTimescaleAndOffset kills the moov.go:329 CONDITIONALS_NEGATION
// (mts != movieTimescale -> the ms-scaling of durMovie is skipped exactly when
// it is needed, for audio tracks) and the moov.go:340 CONDITIONALS_BOUNDARY on
// `s.pts < offsetMs` (finding the minimum, not the maximum, start time).
func TestBuildTrakAudioTimescaleAndOffset(t *testing.T) {
	// pts inserted out of order (600 first, then 100): offsetMs must resolve
	// to the minimum (100), not the first-inserted or the maximum.
	tr := makeAudioOutTrackForMoov(1, 48000, []int64{600, 100})
	trak, presentDur := buildTrak(tr, 1000, false)

	// durMedia = 48000 (0.5s + 0.5s span in media ticks at 48kHz); durMovie
	// must be scaled back to ms (1000), not left in media-timescale units
	// (48000). presentDur = durMovie + offsetMs = 1000 + 100 = 1100.
	if presentDur != 1100 {
		t.Errorf("presentDur = %d, want 1100 (1000ms scaled duration + 100ms offset)", presentDur)
	}

	boxes, err := iterBoxes(trak[8:])
	if err != nil {
		t.Fatalf("iterBoxes(trak): %v", err)
	}
	mdia, ok := findMemBox(boxes, "mdia")
	if !ok {
		t.Fatal("mdia missing")
	}
	mdiaBoxes, _ := iterBoxes(mdia.payload)
	mdhd, ok := findMemBox(mdiaBoxes, "mdhd")
	if !ok {
		t.Fatal("mdhd missing")
	}
	// mdhd v0 payload: version/flags(4) creation(4) modification(4) timescale(4)@12 duration(4)@16
	if ts := binary.BigEndian.Uint32(mdhd.payload[12:16]); ts != 48000 {
		t.Errorf("mdhd timescale = %d, want 48000 (audio track uses its sample rate)", ts)
	}
	if dur := binary.BigEndian.Uint32(mdhd.payload[16:20]); dur != 48000 {
		t.Errorf("mdhd duration = %d, want 48000 (media-timescale units, unscaled)", dur)
	}

	tkhd, ok := findMemBox(boxes, "tkhd")
	if !ok {
		t.Fatal("tkhd missing")
	}
	// tkhd v0 payload: version/flags(4) creation(4) modification(4) track_ID(4) reserved(4) duration(4)@20
	if dur := binary.BigEndian.Uint32(tkhd.payload[20:24]); dur != 1100 {
		t.Errorf("tkhd duration = %d, want 1100 (durMovie scaled to ms, plus the 100ms offset)", dur)
	}
}
