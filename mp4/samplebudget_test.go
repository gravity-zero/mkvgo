package mp4

import (
	"bytes"
	"strings"
	"testing"
)

// TestTrunSampleCountBounded: a few bytes of trun declaring more samples than a movie may hold are refused before
// any of them is appended (a 20-byte trun once asked for 4 billion samples, about 200 GB).
func TestTrunSampleCountBounded(t *testing.T) {
	tfhd := fullBox("tfhd", 0, tfhdDefaultDuration|tfhdDefaultSize, func(w *bw) {
		w.u32(1)
		w.u32(1)
		w.u32(1)
	})
	trun := fullBox("trun", 0, 0, func(w *bw) { w.u32(1<<24 + 1) })
	traf := append(append([]byte{}, tfhd...), trun...)
	mv := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if _, err := parseTrafSamples(traf, 0, -1, 1<<40, nil, mv); err == nil {
		t.Errorf("a trun of %d samples was taken (%d kept)", 1<<24+1, len(mv.tracks[0].samples))
	}
	if n := len(mv.tracks[0].samples); n != 0 {
		t.Errorf("%d samples appended before the refusal", n)
	}
}

// TestMovieSampleTablesBounded: traks whose constant-size tables declare more samples, all together, than a movie
// may hold - each within a size a remote source claims - are kept described, the ones past the bound refused by
// name, and no table past it is expanded.
func TestMovieSampleTablesBounded(t *testing.T) {
	const perTrak = 1 << 22 // five of them pass 1<<24
	table := []([]byte){
		fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(perTrak); w.u32(1) }),
		fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(perTrak); w.u32(1) }),
		fullBox("stsz", 0, 0, func(w *bw) { w.u32(1); w.u32(perTrak) }),
		fullBox("stco", 0, 0, func(w *bw) { w.u32(1); w.u32(0) }),
	}
	esds := esdsBox(0x40, fakeASC)
	var traks [][]byte
	traks = append(traks, mvhdBox(0))
	for id := uint32(1); id <= 5; id++ {
		traks = append(traks, soundTrak(id, 48000, perTrak, audioSampleEntry("mp4a", &stereo48k, esds), table...))
	}
	data := append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), container("moov", traks...)...)
	mv, err := parseMP4(bytes.NewReader(data), 1<<40, sampleFull) // the size a remote source claims
	if err != nil {
		t.Fatal(err)
	}
	total, refused := 0, 0
	for _, tr := range mv.tracks {
		total += len(tr.samples)
		if tr.unsupported != "" {
			refused++
		}
	}
	if total > 1<<24 || refused == 0 {
		t.Errorf("%d samples expanded over %d traks, %d refused; want at most %d and the rest refused", total, len(mv.tracks), refused, 1<<24)
	}
}

// TestSampleBudgetRefusalStatesWhatIsLeft: the refusal compares a table with the budget earlier tracks left, and says so.
func TestSampleBudgetRefusalStatesWhatIsLeft(t *testing.T) {
	budget := int64(10)
	_, err := readTrakTable(firstTrak(t, editFixture(t)), 0, 1<<40, &budget, nil)
	if err == nil || !strings.Contains(err.Error(), "150 samples pass the 10 left") {
		t.Errorf("err = %v", err)
	}
}
