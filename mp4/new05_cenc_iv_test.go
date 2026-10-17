package mp4

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNEW05_CENCIVsDistinctAcrossTracks: under one key, no 'cenc' IV may serve a sample of two tracks, or their keystreams are the same.
func TestNEW05_CENCIVsDistinctAcrossTracks(t *testing.T) {
	src := buildCENCFixture(t)
	for _, ivLen := range []int{8, 16} {
		dir := t.TempDir()
		iv := cencIVFor("cenc")
		iv = append(iv, make([]byte, 16)...)[:ivLen]
		cenc := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: iv, KeyURI: "https://example.test/key"}
		if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, CENC: cenc}); err != nil {
			t.Fatal(err)
		}
		owner := map[string]string{}
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		segs := 0
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".m4s") {
				continue
			}
			track := "video"
			if strings.HasPrefix(e.Name(), "seg_a") {
				track = e.Name()[:strings.LastIndex(e.Name(), "_")]
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			segs++
			for _, s := range parseCENCSegmentFile(t, data, ivLen).samples {
				k := string(s.iv)
				if prev, ok := owner[k]; ok && prev != track {
					t.Fatalf("%d-byte IV %x serves a sample of %s and of %s under the same key", ivLen, s.iv, prev, track)
				}
				owner[k] = track
			}
		}
		if segs < 2 || len(owner) < 2 {
			t.Fatalf("%d segments, %d IVs: the fixture does not exercise two tracks", segs, len(owner))
		}
	}
}

// TestNEW06_CENCIVsDistinctAcrossVariants: ABR variants share the key, so their 'cenc' IVs must differ too, in the full pass and the plan alike.
func TestNEW06_CENCIVsDistinctAcrossVariants(t *testing.T) {
	a, b := buildCENCFixture(t), buildCENCFixture(t)
	cenc := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), KeyURI: "https://k/"}
	o := Options{SegmentMs: 1000, CENC: cenc}
	dir := t.TempDir()
	if err := RemuxToABR(context.Background(), []string{a, b}, dir, o); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanABR(context.Background(), []string{a, b}, o)
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{}
	for _, v := range []string{"v1", "v2"} {
		names, _ := filepath.Glob(filepath.Join(dir, v, "seg0*.m4s"))
		if len(names) == 0 {
			t.Fatalf("%s: no video segment", v)
		}
		for _, name := range names {
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			res, _, err := plan.Resource(context.Background(), v+"/"+filepath.Base(name))
			if err != nil {
				t.Fatal(err)
			}
			if string(res) != string(data) {
				t.Fatalf("%s/%s: the plan and the full pass differ", v, filepath.Base(name))
			}
			for _, s := range parseCENCSegmentFile(t, data, 8).samples {
				if prev, ok := owner[string(s.iv)]; ok && prev != v {
					t.Fatalf("IV %x serves a sample of %s and of %s under the same key", s.iv, prev, v)
				}
				owner[string(s.iv)] = v
			}
		}
	}
}
