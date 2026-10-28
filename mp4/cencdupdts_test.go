package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestCENCIVsDistinctWithinATrack: a 'cenc' sample's IV was the base plus its decode time, and two video blocks
// stamped alike - a broken remux's duplicate timestamp - took one IV, and their AES-CTR keystream with it. The IV
// comes from the sample's place in the source: every sample of a track takes one of its own, in the full pass and
// the plan alike.
func TestCENCIVsDistinctWithinATrack(t *testing.T) {
	w, h := uint32(64), uint32(48)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		pts := int64(i) * 40
		if i == 11 {
			pts = 10 * 40 // frame 11 stamped like frame 10
		}
		blocks = append(blocks, genBlock{track: 1, pts: pts, key: i%25 == 0, data: cencVideoSample()})
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	o := Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), KeyURI: "https://k/"}}
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, o); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, o)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "seg0*.m4s"))
	seen := map[string]string{}
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		res, _, err := plan.Resource(context.Background(), filepath.Base(name))
		if err != nil {
			t.Fatal(err)
		}
		if string(res) != string(data) {
			t.Errorf("%s: the plan and the full pass differ", filepath.Base(name))
		}
		for i, s := range parseCENCSegmentFile(t, data, 8).samples {
			if prev, ok := seen[string(s.iv)]; ok {
				t.Errorf("IV %x serves %s and sample %d of %s", s.iv, prev, i, filepath.Base(name))
			}
			seen[string(s.iv)] = filepath.Base(name)
		}
	}
	if len(seen) < 100 {
		t.Errorf("%d distinct IVs for 100 samples", len(seen))
	}
}

// TestCENCIVFromTheSourceOffset: the IV is the base plus where the sample's bytes sit in the source; a sample with no
// place there (a converted frame), or one past 2^48 bytes, where the variant offsets (2^48 up) and the track domains
// (2^56 up) start, is refused under 'cenc'.
func TestCENCIVFromTheSourceOffset(t *testing.T) {
	k, err := newCENCCipher(&CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), InlineKey: true}, false, "aac", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	a, err := k.entry([]byte{1}, 100)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.entry([]byte{2}, 101)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.iv) == string(b.iv) {
		t.Error("two samples at two places took one IV")
	}
	if _, err := k.entry([]byte{1}, 0); err == nil {
		t.Error("a sample with no place in the source took an IV")
	}
	if _, err := k.entry([]byte{1}, 1<<48); err == nil {
		t.Error("a sample 2^48 bytes into the source took an IV")
	}
}
