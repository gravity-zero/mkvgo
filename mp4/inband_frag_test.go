package mp4

import (
	"bytes"
	"testing"
)

// TestFragmentedHeadSamples: a fragmented movie read head-only has empty sample tables, so the first-sample
// fallback had nothing to read (an avc3 or hev1 track kept no profile). The first moof gives each track the place
// of its first samples, the same a full parse finds, and the movie keeps no sample of it.
func TestFragmentedHeadSamples(t *testing.T) {
	data, _ := makeFragmentedMP4(t)
	full, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleFull)
	if err != nil {
		t.Fatal(err)
	}
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), sampleNone)
	if err != nil {
		t.Fatal(err)
	}
	vt, want := videoTrack(mv), videoTrack(full)
	if vt == nil || want == nil || len(want.samples) < 2 {
		t.Fatal("fixture: no video samples")
	}
	if vt.firstSampleSize != 0 || len(vt.samples) != 0 {
		t.Fatalf("head-only: first sample size %d, %d samples; want none", vt.firstSampleSize, len(vt.samples))
	}
	endBefore := vt.fragDecodeEnd
	fragHeadSamples(bytes.NewReader(data), mv)
	if vt.firstSampleOffset != want.samples[0].offset || vt.firstSampleSize != want.samples[0].size {
		t.Errorf("first sample at %d (%d bytes), want %d (%d)", vt.firstSampleOffset, vt.firstSampleSize, want.samples[0].offset, want.samples[0].size)
	}
	if len(vt.headSamples) < 2 || len(vt.headSamples) > headSampleCount {
		t.Fatalf("%d head samples, want 2..%d", len(vt.headSamples), headSampleCount)
	}
	for i, l := range vt.headSamples {
		if l.off != want.samples[i].offset || l.size != want.samples[i].size {
			t.Errorf("head sample %d at %d (%d bytes), want %d (%d)", i, l.off, l.size, want.samples[i].offset, want.samples[i].size)
		}
	}
	if len(vt.samples) != 0 || vt.fragDecodeEnd != endBefore || mv.trackIdx != nil {
		t.Errorf("the movie kept %d samples, decode end %d (was %d): the head-only read must stay as it was", len(vt.samples), vt.fragDecodeEnd, endBefore)
	}

	// After a full parse the samples are already here: no fragment is read again.
	fv := videoTrack(full)
	fv.firstSampleSize, fv.headSamples = 0, nil
	fragHeadSamples(bytes.NewReader(nil), full)
	if fv.firstSampleSize != want.samples[0].size || len(fv.headSamples) == 0 {
		t.Errorf("after a full parse: first sample size %d, %d head samples", fv.firstSampleSize, len(fv.headSamples))
	}
}
