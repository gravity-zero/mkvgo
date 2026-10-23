package mp4

import "testing"

// TestSaizStatesEachSencEntryWhole: a picture of more NAL units than one saiz byte can size still gets an entry saiz
// states exactly, every NAL length and header left clear.
func TestSaizStatesEachSencEntryWhole(t *testing.T) {
	sample := lengthPrefixed(4, testAVCSPS(), testAVCPPS())
	var starts []int
	for i := range 60 {
		starts = append(starts, len(sample))
		nal, _ := testAVCSlice(uint32(i), 0, 50)
		sample = append(sample, lengthPrefixed(4, nal)...)
	}
	for _, scheme := range []string{"cenc", "cbcs"} {
		k, err := newCENCCipher(&CENCOptions{Scheme: scheme, Key: cencKey, KeyID: cencKID, IV: cencIVFor(scheme), InlineKey: true}, true, "h264", nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		e, err := k.entry(sample, 0)
		if err != nil {
			t.Fatal(err)
		}
		saiz := buildSaiz(&cencTrafData{hasSubsample: true, entries: []sencEntry{e}})
		if want := len(e.iv) + 2 + 6*len(e.subs); int(saiz[len(saiz)-1]) != want {
			t.Errorf("%s: saiz states %d bytes, the senc entry holds %d", scheme, saiz[len(saiz)-1], want)
		}
		clear := make([]bool, len(sample))
		pos := 0
		for _, s := range e.subs {
			for j := pos; j < pos+int(s.clear); j++ {
				clear[j] = true
			}
			pos += int(s.clear) + int(s.protected)
		}
		if pos != len(sample) {
			t.Errorf("%s: the subsamples cover %d bytes of %d", scheme, pos, len(sample))
		}
		for _, at := range starts {
			if !clear[at] || !clear[at+4] {
				t.Errorf("%s: the NAL unit at %d does not keep its length and header clear", scheme, at)
			}
		}
	}
}
