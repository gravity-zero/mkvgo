package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Real H.264 and HEVC encoded from a test pattern: several slices a picture,
// parameter sets and SEI in band, B-frames and weighted prediction, interlaced
// CAVLC (H.264), entry points (HEVC).
var h26xFixtures = []struct{ path, codec string }{
	{"../internal/testdata/h264_slices.mkv", "h264"},
	{"../internal/testdata/h264_interlaced_cavlc.mkv", "h264"},
	{"../internal/testdata/hevc_slices.mkv", "hevc"},
}

// Every segment decrypts to the clear one, its NAL units other than slice data
// read from the ciphertext alone, and the full pass, Resource and Open agree.
func TestCENCRealH26x(t *testing.T) {
	ctx := context.Background()
	for _, f := range h26xFixtures {
		clearDir := t.TempDir()
		if err := RemuxToHLS(ctx, f.path, clearDir, Options{SegmentMs: 1000}); err != nil {
			t.Fatal(err)
		}
		for _, scheme := range []string{"cenc", "cbcs"} {
			t.Run(filepath.Base(f.path)+"/"+scheme, func(t *testing.T) {
				o := Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: scheme, Key: cencKey, KeyID: cencKID, IV: cencIVFor(scheme), InlineKey: true}}
				encDir := t.TempDir()
				if err := RemuxToHLS(ctx, f.path, encDir, o); err != nil {
					t.Fatal(err)
				}
				plan, err := PlanHLS(ctx, f.path, o)
				if err != nil {
					t.Fatal(err)
				}
				assertOpenMatchesResource(t, plan)
				private := plan.tracks[0].ft.outTrack.mkv.CodecPrivate
				ivSize := 0
				if scheme == "cenc" {
					ivSize = len(o.CENC.IV)
				}
				slices := 0
				for n := 1; ; n++ {
					name := fmt.Sprintf("seg%05d.m4s", n)
					enc, err := os.ReadFile(filepath.Join(encDir, name))
					if os.IsNotExist(err) {
						if n < 3 {
							t.Fatalf("%d segments", n-1)
						}
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if res, _, err := plan.Resource(ctx, name); err != nil || !bytes.Equal(res, enc) {
						t.Fatalf("%s: plan differs from the full pass (%v)", name, err)
					}
					plain, err := os.ReadFile(filepath.Join(clearDir, name))
					if err != nil {
						t.Fatal(err)
					}
					ps := parseCENCSegmentFile(t, enc, ivSize)
					cipher := mdatSamples(ps.mdat, ps.sampleSizes)
					clear := mdatSamples(mustBox(t, walkBoxes(t, plain, 0), "mdat").payload, ps.sampleSizes)
					for i, ct := range cipher {
						if got := decryptSample(t, scheme, cencKey, true, ct, ps.samples[i], o.CENC.IV); !bytes.Equal(got, clear[i]) {
							t.Fatalf("%s sample %d: decrypted != clear", name, i)
						}
						sp, err := newH26xSplitter(f.codec, private, scheme)
						if err != nil {
							t.Fatal(err)
						}
						subs, err := sp.split(ct)
						if err != nil {
							t.Fatalf("%s sample %d: unreadable without the key: %v", name, i, err)
						}
						if len(subs) != len(ps.samples[i].subs) {
							t.Fatalf("%s sample %d: %d subsamples from the ciphertext, senc %d", name, i, len(subs), len(ps.samples[i].subs))
						}
						for k, s := range subs {
							if int(s.clear) != ps.samples[i].subs[k][0] || int(s.protected) != ps.samples[i].subs[k][1] {
								t.Fatalf("%s sample %d subsample %d: %+v from the ciphertext, senc %v", name, i, k, s, ps.samples[i].subs[k])
							}
							if scheme == "cenc" && s.protected%16 != 0 {
								t.Fatalf("%s sample %d: %d protected bytes, not whole AES blocks", name, i, s.protected)
							}
							if s.protected > 0 {
								slices++
							}
						}
					}
				}
				if slices < 50 {
					t.Errorf("%d protected slices: the fixture does not exercise the slice headers", slices)
				}
			})
		}
	}
}

// A picture parameter set redefined in band changes the slice header it
// governs: the segment that carries it reads its slices against it, packaged
// alone or in a full pass.
func TestCENCParameterSetRedefinedInBand(t *testing.T) {
	w, h := uint32(64), uint32(48)
	var blocks []genBlock
	lead, sliceLen := map[bool]int{}, map[bool]int{}
	for i := 0; i < 50; i++ {
		redundant := i >= 25
		slice, hdr := testAVCSliceWith(0, 0, 200, redundant)
		var nals [][]byte
		if i%25 == 0 {
			nals = append(nals, testAVCSPS(), testAVCPPSWith(redundant))
		}
		sample := lengthPrefixed(4, append(nals, slice)...)
		lead[redundant], sliceLen[redundant] = 4+hdr, len(slice)
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: sample})
	}
	if lead[true] <= lead[false]+1 {
		t.Fatalf("slice headers of %d and %d bytes: the test tells nothing", lead[false], lead[true])
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, blocks)
	ctx := context.Background()
	psLen := 4 + len(testAVCSPS()) + 4 + len(testAVCPPS())
	for _, scheme := range []string{"cbcs", "cenc"} {
		o := Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: scheme, Key: cencKey, KeyID: cencKID, IV: cencIVFor(scheme), InlineKey: true}}
		dir := t.TempDir()
		if err := RemuxToHLS(ctx, src, dir, o); err != nil {
			t.Fatal(err)
		}
		plan, err := PlanHLS(ctx, src, o)
		if err != nil {
			t.Fatal(err)
		}
		assertOpenMatchesResource(t, plan)
		ivSize := 0
		if scheme == "cenc" {
			ivSize = len(o.CENC.IV)
		}
		for n, redundant := range []bool{false, true} {
			name := fmt.Sprintf("seg%05d.m4s", n+1)
			full, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if alone, _, err := plan.Resource(ctx, name); err != nil || !bytes.Equal(alone, full) {
				t.Fatalf("%s %s: packaged alone, it differs from the full pass (%v)", scheme, name, err)
			}
			for i, e := range parseCENCSegmentFile(t, full, ivSize).samples {
				want := lead[redundant]
				if i == 0 {
					want += psLen
				}
				if scheme == "cenc" {
					want += (sliceLen[redundant] + 4 - lead[redundant]) % 16
				}
				if len(e.subs) != 1 || e.subs[0][0] != want {
					t.Fatalf("%s %s sample %d: subsamples %v, want %d clear bytes", scheme, name, i, e.subs, want)
				}
			}
		}
	}
}
