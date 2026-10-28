package mp4

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// segmentOrders are four orders a player, or several, may ask a presentation's segments in: in turn, backwards, the
// odd ones first, from the middle on and round.
func segmentOrders(names []string) [][]string {
	n := len(names)
	asc := append([]string(nil), names...)
	desc := make([]string, n)
	for i := range names {
		desc[i] = names[n-1-i]
	}
	var oddFirst []string
	for _, start := range []int{1, 0} {
		for i := start; i < n; i += 2 {
			oddFirst = append(oddFirst, names[i])
		}
	}
	rot := append(append([]string(nil), names[n/2:]...), names[:n/2]...)
	return [][]string{asc, desc, oddFirst, rot}
}

// cencPlaintexts decrypts every sample of a 'cenc' segment, keyed by its IV.
func cencPlaintexts(t *testing.T, name string, seg []byte, ivSize int, ivs map[string][32]byte, owner map[string]string) {
	t.Helper()
	block, err := aes.NewCipher(cencKey)
	if err != nil {
		t.Fatal(err)
	}
	ps := parseCENCSegmentFile(t, seg, ivSize)
	pos := 0
	for i, size := range ps.sampleSizes {
		data := append([]byte(nil), ps.mdat[pos:pos+int(size)]...)
		pos += int(size)
		s := ps.samples[i]
		var subs []cencSubsample
		for _, sub := range s.subs {
			subs = append(subs, cencSubsample{clear: uint16(sub[0]), protected: uint32(sub[1])})
		}
		cencCTREncrypt(block, cencCounterBlock(s.iv), data, subs) // CTR: encrypting again decrypts
		sum := sha256.Sum256(data)
		if prev, ok := ivs[string(s.iv)]; ok && prev != sum {
			t.Errorf("IV %x encrypts two plaintexts: %s and sample %d of %s", s.iv, owner[string(s.iv)], i, name)
		}
		ivs[string(s.iv)], owner[string(s.iv)] = sum, name
	}
}

// TestCENCSegmentsSameInEveryOrder: every 'cenc' segment of an MKV and of an MP4 source, through the full pass,
// Resource and Open, asked in four orders on a fresh plan and again on the same plan, is the same bytes; and no IV
// encrypts two different plaintexts. The IV was the sample's decode time, which two samples could share and an
// on-demand window could time by what was built before it.
func TestCENCSegmentsSameInEveryOrder(t *testing.T) {
	ctx := context.Background()
	mkvSrc := buildCENCFixture(t)
	mp4Src := filepath.Join(t.TempDir(), "src.mp4")
	if err := RemuxToMP4(ctx, mkvSrc, mp4Src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []struct{ name, path string }{{"mkv", mkvSrc}, {"mp4", mp4Src}} {
		for _, iv := range [][]byte{cencIV8, cencIV16} {
			label := src.name + "/iv" + string(rune('0'+len(iv)/8))
			o := Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: iv, KeyURI: "https://k/"}}
			dir := t.TempDir()
			if err := RemuxToHLS(ctx, src.path, dir, o); err != nil {
				t.Fatalf("%s: full pass: %v", label, err)
			}
			ref, err := PlanHLS(ctx, src.path, o)
			if err != nil {
				t.Fatal(err)
			}
			full := map[string][]byte{}
			var names []string
			for _, name := range ref.Resources() {
				if !strings.HasSuffix(name, ".m4s") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				full[name] = data
				names = append(names, name)
			}
			if len(names) < 4 {
				t.Fatalf("%s: %d segments", label, len(names))
			}
			ivs, owner := map[string][32]byte{}, map[string]string{}
			for _, name := range names {
				cencPlaintexts(t, label+" "+name, full[name], len(iv), ivs, owner)
			}
			serve := map[string]func(p *HLSPlan, name string) ([]byte, error){
				"Resource": func(p *HLSPlan, name string) ([]byte, error) {
					b, _, err := p.Resource(ctx, name)
					return b, err
				},
				"Open": func(p *HLSPlan, name string) ([]byte, error) {
					h, err := p.Open(ctx, name)
					if err != nil {
						return nil, err
					}
					defer h.Close()
					var out bytes.Buffer
					_, err = h.WriteTo(ctx, &out)
					return out.Bytes(), err
				},
			}
			for via, get := range serve {
				for k, order := range segmentOrders(names) {
					p, err := PlanHLS(ctx, src.path, o)
					if err != nil {
						t.Fatal(err)
					}
					for pass, seq := range [][]string{order, segmentOrders(order)[1]} { // cold, then warm in reverse
						for _, name := range seq {
							got, err := get(p, name)
							if err != nil {
								t.Fatalf("%s %s order %d pass %d %s: %v", label, via, k, pass, name, err)
							}
							if !bytes.Equal(got, full[name]) {
								t.Errorf("%s %s order %d pass %d: %s differs from the full pass", label, via, k, pass, name)
							}
						}
					}
				}
			}
		}
	}
}

// TestCENCRefusesConvertedSamples: a frame an Options.FrameConverter converted is not the source's bytes and has no
// place in it to derive a 'cenc' IV from; the full pass refuses it, by name.
func TestCENCRefusesConvertedSamples(t *testing.T) {
	src := buildCENCFixture(t)
	o := Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "https://k/"},
		FrameConverter: identityConverter{}}
	err := RemuxToHLS(context.Background(), src, t.TempDir(), o)
	if err == nil || !strings.Contains(err.Error(), "cbcs") {
		t.Errorf("a converted track under 'cenc': %v, want a refusal naming cbcs", err)
	}
}

// transientGridSource is 12 s of video with AAC audio on its 1024-sample grid, whose stored timecodes run 23 ms
// ahead of it from 3.7 s to the 4 s boundary.
func transientGridSource(t testing.TB) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 300; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := 0; int64(i)*1024*1000/44100 < 12000; i++ {
		ms := int64(i) * 1024 * 1000 / 44100
		if ms >= 3700 && ms < 3950 {
			ms += 23
		}
		blocks = append(blocks, genBlock{track: 2, pts: ms, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(blocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, DefaultDurationNs: 1024 * 1_000_000_000 / 44100},
	}, blocks)
}

// TestCENCIVOncePerPlaintextInEveryOrder: grid-timed audio whose timecodes run ahead of the grid is timed on demand
// by the window built first, so a sample served in one order takes the decode time its neighbour has in another. The
// IV was that decode time: two clients asking in two orders got two plaintexts under one IV. No IV, across every
// order, encrypts two plaintexts.
func TestCENCIVOncePerPlaintextInEveryOrder(t *testing.T) {
	ctx := context.Background()
	src := transientGridSource(t)
	o := Options{SegmentMs: 2000, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "https://k/"}}
	ref, err := PlanHLS(ctx, src, o)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, name := range ref.Resources() {
		if strings.HasSuffix(name, ".m4s") {
			names = append(names, name)
		}
	}
	ivs, owner := map[string][32]byte{}, map[string]string{}
	for k, order := range segmentOrders(names) {
		p, err := PlanHLS(ctx, src, o)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range order {
			seg, _, err := p.Resource(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			cencPlaintexts(t, name+" in order "+string(rune('0'+k)), seg, len(cencIV8), ivs, owner)
		}
	}
}

// TestGridSegmentsSameInEveryOrder: grid-timed audio whose timecodes leave the grid across a boundary was timed, on
// demand, from whichever of the two windows was built first: the same segment came out in two forms depending on the
// order it and the one before were asked in. Every segment is the full pass's bytes in every order, through Resource
// and Open, cold and warm.
func TestGridSegmentsSameInEveryOrder(t *testing.T) {
	ctx := context.Background()
	for _, src := range []struct {
		name string
		path string
	}{{"behind", jitteredGridSource(t)}} {
		o := Options{SegmentMs: 2000}
		dir := t.TempDir()
		if err := RemuxToHLS(ctx, src.path, dir, o); err != nil {
			t.Fatal(err)
		}
		ref, err := PlanHLS(ctx, src.path, o)
		if err != nil {
			t.Fatal(err)
		}
		full := map[string][]byte{}
		var names []string
		for _, name := range ref.Resources() {
			if strings.HasSuffix(name, ".m4s") {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				full[name], names = data, append(names, name)
			}
		}
		for k, order := range segmentOrders(names) {
			for _, via := range []string{"Resource", "Open"} {
				p, err := PlanHLS(ctx, src.path, o)
				if err != nil {
					t.Fatal(err)
				}
				for pass, seq := range [][]string{order, segmentOrders(order)[1]} {
					for _, name := range seq {
						var got []byte
						if via == "Resource" {
							got, _, err = p.Resource(ctx, name)
						} else {
							var h *ResourceHandle
							if h, err = p.Open(ctx, name); err == nil {
								var out bytes.Buffer
								_, err = h.WriteTo(ctx, &out)
								h.Close()
								got = out.Bytes()
							}
						}
						if err != nil {
							t.Fatalf("%s %s order %d %s: %v", src.name, via, k, name, err)
						}
						if !bytes.Equal(got, full[name]) {
							t.Errorf("%s %s order %d pass %d: %s differs from the full pass", src.name, via, k, pass, name)
						}
					}
				}
			}
		}
	}
}
