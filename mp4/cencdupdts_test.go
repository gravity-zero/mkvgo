package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestCENCIVsDistinctWithinATrack: a 'cenc' sample's IV is the base plus its decode time, and the decode times were
// the sorted presentation times as they are: two video blocks stamped alike - a broken remux's duplicate timestamp -
// took one IV, and their AES-CTR keystream with it. Every sample of a track takes an IV of its own now, in the full
// pass and the plan alike.
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

// TestCENCRefusesARepeatedIV: whatever derived the decode times - an MP4 source's stts may hold a zero delta - two
// samples of a fragment at one decode time are refused under 'cenc' rather than encrypted with one IV.
func TestCENCRefusesARepeatedIV(t *testing.T) {
	k, err := newCENCCipher(&CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), InlineKey: true}, false, "aac", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.entry([]byte{1}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := k.entry([]byte{2}, 100); err == nil {
		t.Error("a second sample at decode time 100 took an IV, want it refused")
	}
}

// TestCENCRefusesADecodeTimePastTheDomains: the IV is the base plus the decode time plus the track's domain (2^56
// up) or the variant's offset (2^48 up); a forged stts reaching 2^48 ticks would walk one track's IVs into another's.
func TestCENCRefusesADecodeTimePastTheDomains(t *testing.T) {
	k, err := newCENCCipher(&CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), InlineKey: true}, false, "aac", nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.entry([]byte{1}, 1<<48); err == nil {
		t.Error("a decode time of 2^48 ticks took an IV, want it refused")
	}
}
