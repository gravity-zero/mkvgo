package mp4

// cenc_test.go - Common Encryption round-trip proof: a test-side decryptor
// (CTR and CBC+pattern, subsample-aware, driven only by the produced boxes -
// tenc/senc/saiz/saio) that decrypts real packaged output back to the
// unencrypted bytes, plus saio-offset, clear-region, pattern, byte-parity,
// signaling and refusal coverage. Independent of cenc.go's own encrypt
// helpers (crypto/aes + crypto/cipher primitives only), so this validates the
// packaging output itself, not the encoder against itself.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

var (
	cencKey  = []byte("0123456789abcdef") // 16 bytes
	cencKID  = []byte("KEYID-0123456789") // 16 bytes
	cencIV8  = []byte{0, 0, 0, 0, 0, 0, 0, 1}
	cencIV16 = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
)

// cencIVFor returns the scheme-appropriate base IV.
func cencIVFor(scheme string) []byte {
	if scheme == "cenc" {
		return cencIV8
	}
	return cencIV16
}

// avccNAL prepends a 4-byte big-endian length to a NAL unit's bytes (payload
// already includes the NAL header byte(s)).
func avccNAL(payload []byte) []byte {
	var b []byte
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(payload)))
	b = append(b, l[:]...)
	b = append(b, payload...)
	return b
}

// cencVideoNAL1 / cencVideoNAL2 are the two NAL units of every synthetic H.264
// sample below: a tiny one (SPS-ish) and a big one (300-byte payload, > 160
// bytes protected - enough to see several cbcs pattern cycles).
func cencVideoNAL1() []byte { return []byte{0x67, 0xAA, 0xBB, 0xCC} }
func cencVideoNAL2() []byte {
	b := make([]byte, 301)
	b[0] = 0x65
	for i := 1; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

func cencVideoSample() []byte {
	return append(avccNAL(cencVideoNAL1()), avccNAL(cencVideoNAL2())...)
}

func cencAudioSample(i int) []byte {
	b := make([]byte, 50)
	for j := range b {
		b[j] = byte(i*7 + j)
	}
	return b
}

// buildCENCFixture writes a synthetic h264+aac source: 100 video frames at
// 40ms (a keyframe every 25th, so at 0/1000/2000/3000ms - the same shape
// TestHLSEncryptionAndRewrite uses, which spans enough ~1s clusters for each
// keyframe to land in its own Cues entry) and matching-duration audio.
func buildCENCFixture(t *testing.T) string {
	t.Helper()
	w, h := uint32(64), uint32(48)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 100; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := 0; i < 200; i++ {
		blocks = append(blocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: cencAudioSample(i)})
	}
	sortGenBlocks(blocks)
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		blocks)
}

// --- test-side sinf/tenc reader ---------------------------------------------

type tencInfo struct {
	origType          string
	scheme            string
	version           int
	ivSize            int
	cryptBlk, skipBlk byte
	kid               []byte
	constIV           []byte
}

// parseSinfTenc walks one rendition's init segment down to its sample entry's
// sinf/tenc - the box path is moov > trak > mdia > minf > stbl > stsd >
// encv|enca > sinf > (frma, schm, schi > tenc).
func parseSinfTenc(t *testing.T, initData []byte, isVideo bool) tencInfo {
	t.Helper()
	top := walkBoxes(t, initData, 0)
	moov := mustBox(t, top, "moov")
	moovBoxes := walkBoxes(t, moov.payload, moov.dataOff)
	trak := mustBox(t, moovBoxes, "trak")
	trakBoxes := walkBoxes(t, trak.payload, trak.dataOff)
	mdia := mustBox(t, trakBoxes, "mdia")
	mdiaBoxes := walkBoxes(t, mdia.payload, mdia.dataOff)
	minf := mustBox(t, mdiaBoxes, "minf")
	minfBoxes := walkBoxes(t, minf.payload, minf.dataOff)
	stbl := mustBox(t, minfBoxes, "stbl")
	stblBoxes := walkBoxes(t, stbl.payload, stbl.dataOff)
	stsd := mustBox(t, stblBoxes, "stsd")
	entries := walkBoxes(t, stsd.payload[8:], stsd.dataOff+8)
	if len(entries) == 0 {
		t.Fatal("stsd has no sample entry")
	}
	entry := entries[0]
	prefix := 28
	wantType := "enca"
	if isVideo {
		prefix, wantType = 78, "encv"
	}
	if entry.typ != wantType {
		t.Fatalf("sample entry type = %q, want %q", entry.typ, wantType)
	}
	if len(entry.payload) < prefix {
		t.Fatalf("sample entry too short (%d bytes)", len(entry.payload))
	}
	children := walkBoxes(t, entry.payload[prefix:], entry.dataOff+int64(prefix))
	sinf := mustBox(t, children, "sinf")
	sinfBoxes := walkBoxes(t, sinf.payload, sinf.dataOff)
	frma := mustBox(t, sinfBoxes, "frma")
	schm := mustBox(t, sinfBoxes, "schm")
	schi := mustBox(t, sinfBoxes, "schi")
	schiBoxes := walkBoxes(t, schi.payload, schi.dataOff)
	tenc := mustBox(t, schiBoxes, "tenc")
	// tenc.payload is a FullBox payload: version(1) + flags(3) + content - the
	// TrackEncryptionBox fields (reserved/pattern/isProtected/ivSize/KID/…)
	// start at byte 4, not 0.
	p := tenc.payload
	c := p[4:]

	info := tencInfo{origType: string(frma.payload[:4]), scheme: string(schm.payload[4:8]), version: int(p[0])}
	if info.version == 0 {
		info.ivSize = int(c[3])
		info.kid = append([]byte(nil), c[4:20]...)
	} else {
		info.cryptBlk = c[1] >> 4
		info.skipBlk = c[1] & 0x0F
		info.ivSize = int(c[3])
		info.kid = append([]byte(nil), c[4:20]...)
		if len(c) > 20 {
			n := int(c[20])
			info.constIV = append([]byte(nil), c[21:21+n]...)
		}
	}
	return info
}

// --- test-side senc/saiz/saio/trun reader -----------------------------------

type sencSample struct {
	iv   []byte
	subs [][2]int // [clear, protected] per NAL
}

type parsedSeg struct {
	moofStart    int64 // absolute offset of the moof box's OWN first byte
	sampleSizes  []uint32
	hasSubsample bool
	samples      []sencSample // nil when the segment carries no senc
	saioOffset   int64
	sencIVAbsOff int64 // independently computed: where the first IV byte actually is
	mdat         []byte
}

func parseTrunSizes(payload []byte) []uint32 {
	flags := uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
	count := binary.BigEndian.Uint32(payload[4:8])
	pos := 8
	if flags&0x000001 != 0 {
		pos += 4
	}
	if flags&0x000004 != 0 {
		pos += 4
	}
	var sizes []uint32
	for i := uint32(0); i < count; i++ {
		if flags&0x000100 != 0 {
			pos += 4
		}
		if flags&0x000200 != 0 {
			sizes = append(sizes, binary.BigEndian.Uint32(payload[pos:pos+4]))
			pos += 4
		}
		if flags&0x000400 != 0 {
			pos += 4
		}
		if flags&0x000800 != 0 {
			pos += 4
		}
	}
	return sizes
}

func parseCENCSegmentFile(t *testing.T, seg []byte, ivSize int) parsedSeg {
	t.Helper()
	top := walkBoxes(t, seg, 0)
	moof := mustBox(t, top, "moof")
	ps := parsedSeg{moofStart: moof.dataOff - 8} // small-form assumption (segments are tiny)
	moofBoxes := walkBoxes(t, moof.payload, moof.dataOff)
	traf := mustBox(t, moofBoxes, "traf")
	trafBoxes := walkBoxes(t, traf.payload, traf.dataOff)
	trun := mustBox(t, trafBoxes, "trun")
	ps.sampleSizes = parseTrunSizes(trun.payload)
	mdat := mustBox(t, top, "mdat")
	ps.mdat = mdat.payload

	if senc, ok := findBox(trafBoxes, "senc"); ok {
		flags := uint32(senc.payload[1])<<16 | uint32(senc.payload[2])<<8 | uint32(senc.payload[3])
		ps.hasSubsample = flags&0x000002 != 0
		count := binary.BigEndian.Uint32(senc.payload[4:8])
		pos := 8
		ps.sencIVAbsOff = senc.dataOff + 8
		for i := uint32(0); i < count; i++ {
			var s sencSample
			if ivSize > 0 {
				s.iv = append([]byte(nil), senc.payload[pos:pos+ivSize]...)
				pos += ivSize
			}
			if ps.hasSubsample {
				n := int(binary.BigEndian.Uint16(senc.payload[pos : pos+2]))
				pos += 2
				for k := 0; k < n; k++ {
					clear := int(binary.BigEndian.Uint16(senc.payload[pos : pos+2]))
					protected := int(binary.BigEndian.Uint32(senc.payload[pos+2 : pos+6]))
					s.subs = append(s.subs, [2]int{clear, protected})
					pos += 6
				}
			}
			ps.samples = append(ps.samples, s)
		}
	}
	if saio, ok := findBox(trafBoxes, "saio"); ok {
		// FullBox payload: version(1)+flags(3)+entry_count(4)+offset(4, v0).
		ps.saioOffset = int64(binary.BigEndian.Uint32(saio.payload[8:12]))
	}
	return ps
}

// mdatSamples slices a segment's mdat payload per its trun sample sizes, in
// order (one track per rendition file: samples are contiguous, no interleave).
func mdatSamples(mdat []byte, sizes []uint32) [][]byte {
	var out [][]byte
	pos := 0
	for _, sz := range sizes {
		out = append(out, mdat[pos:pos+int(sz)])
		pos += int(sz)
	}
	return out
}

// --- test-side decryption ----------------------------------------------------

func cencCTRDecryptSample(t *testing.T, key, ivField []byte, sample []byte, subs [][2]int) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	counter := make([]byte, aes.BlockSize)
	copy(counter, ivField) // 16 bytes used directly; 8 bytes zero-pad the low half
	stream := cipher.NewCTR(block, counter)
	out := append([]byte(nil), sample...)
	if len(subs) == 0 {
		stream.XORKeyStream(out, out)
		return out
	}
	pos := 0
	for _, s := range subs {
		pos += s[0]
		if s[1] > 0 {
			stream.XORKeyStream(out[pos:pos+s[1]], out[pos:pos+s[1]])
			pos += s[1]
		}
	}
	return out
}

// cbcsPatternDecrypt mirrors cenc.go's cbcsPatternEncrypt but decrypts,
// capturing each crypt run's CIPHERTEXT (not the plaintext CryptBlocks
// produces) as the next run's chain value - the correct CBC inverse.
func cbcsPatternDecrypt(t *testing.T, key, iv []byte, region []byte, cryptBlocks, skipBlocks int) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte(nil), region...)
	full := len(out) / aes.BlockSize * aes.BlockSize
	chain := append([]byte(nil), iv...)
	pos := 0
	for pos < full {
		n := cryptBlocks * aes.BlockSize
		if pos+n > full {
			n = full - pos
		}
		if n > 0 {
			lastCipher := append([]byte(nil), out[pos+n-aes.BlockSize:pos+n]...)
			cipher.NewCBCDecrypter(block, chain).CryptBlocks(out[pos:pos+n], out[pos:pos+n])
			chain = lastCipher
			pos += n
		}
		if skipBlocks <= 0 {
			if n == 0 {
				break
			}
			continue
		}
		skip := skipBlocks * aes.BlockSize
		if pos+skip > full {
			skip = full - pos
		}
		pos += skip
	}
	return out
}

func decryptSample(t *testing.T, scheme string, key []byte, isVideo bool, sample []byte, s sencSample, constIV []byte) []byte {
	t.Helper()
	if scheme == "cenc" {
		return cencCTRDecryptSample(t, key, s.iv, sample, s.subs)
	}
	if isVideo && len(s.subs) > 0 {
		out := append([]byte(nil), sample...)
		pos := 0
		for _, sub := range s.subs {
			pos += sub[0]
			if sub[1] > 0 {
				dec := cbcsPatternDecrypt(t, key, constIV, out[pos:pos+sub[1]], 1, 9)
				copy(out[pos:pos+sub[1]], dec)
				pos += sub[1]
			}
		}
		return out
	}
	return cbcsPatternDecrypt(t, key, constIV, sample, len(sample)/aes.BlockSize, 0)
}

// --- the round-trip test -----------------------------------------------------

func TestCENCRoundTrip(t *testing.T) {
	for _, scheme := range []string{"cenc", "cbcs"} {
		t.Run(scheme, func(t *testing.T) {
			src := buildCENCFixture(t)
			cenc := &CENCOptions{Scheme: scheme, Key: cencKey, KeyID: cencKID, IV: cencIVFor(scheme), KeyURI: "https://example.test/key"}

			clearDir, cencDir := t.TempDir(), t.TempDir()
			if err := RemuxToHLS(context.Background(), src, clearDir, Options{SegmentMs: 1000}); err != nil {
				t.Fatal(err)
			}
			if err := RemuxToHLS(context.Background(), src, cencDir, Options{SegmentMs: 1000, CENC: cenc}); err != nil {
				t.Fatal(err)
			}

			renditions := []struct {
				name    string
				init    string
				seg1    string
				isVideo bool
			}{
				{"video", "init.mp4", "seg00001.m4s", true},
				{"audio", "init_a1.mp4", "seg_a1_00001.m4s", false},
			}
			for _, r := range renditions {
				t.Run(r.name, func(t *testing.T) {
					initData, err := os.ReadFile(filepath.Join(cencDir, r.init))
					if err != nil {
						t.Fatal(err)
					}
					info := parseSinfTenc(t, initData, r.isVideo)
					wantOrig := "mp4a"
					if r.isVideo {
						wantOrig = "avc1"
					}
					if info.origType != wantOrig {
						t.Errorf("frma = %q, want %q", info.origType, wantOrig)
					}
					if info.scheme != scheme {
						t.Errorf("schm scheme = %q, want %q", info.scheme, scheme)
					}
					wantIVSize, wantConstIV := 0, cenc.IV
					if scheme == "cenc" {
						wantIVSize, wantConstIV = len(cenc.IV), nil
					}
					if info.ivSize != wantIVSize {
						t.Errorf("tenc Per_Sample_IV_Size = %d, want %d", info.ivSize, wantIVSize)
					}
					if !bytes.Equal(info.kid, cencKID) {
						t.Errorf("tenc default_KID = %x, want %x", info.kid, cencKID)
					}
					if wantConstIV != nil && !bytes.Equal(info.constIV, wantConstIV) {
						t.Errorf("tenc default_constant_IV = %x, want %x", info.constIV, wantConstIV)
					}
					if scheme == "cbcs" && r.isVideo && (info.cryptBlk != 1 || info.skipBlk != 9) {
						t.Errorf("cbcs video pattern = %d:%d, want 1:9", info.cryptBlk, info.skipBlk)
					}
					if scheme == "cbcs" && !r.isVideo && (info.cryptBlk != 0 || info.skipBlk != 0) {
						t.Errorf("cbcs audio pattern = %d:%d, want 0:0 (whole sample)", info.cryptBlk, info.skipBlk)
					}

					// Every segment: decrypt and compare against the clear run.
					for segN := 1; segN <= 4; segN++ {
						cName := r.seg1
						if segN > 1 {
							cName = seg1To(r.seg1, segN)
						}
						cData, err := os.ReadFile(filepath.Join(cencDir, cName))
						if err != nil {
							t.Fatalf("segment %d: %v", segN, err)
						}
						pData, err := os.ReadFile(filepath.Join(clearDir, cName))
						if err != nil {
							t.Fatalf("clear segment %d: %v", segN, err)
						}
						ps := parseCENCSegmentFile(t, cData, info.ivSize)
						clearTop := walkBoxes(t, pData, 0)
						clearMdat := mustBox(t, clearTop, "mdat")

						cipherSamples := mdatSamples(ps.mdat, ps.sampleSizes)
						plainSamples := mdatSamples(clearMdat.payload, ps.sampleSizes)
						if len(ps.samples) != len(cipherSamples) {
							t.Fatalf("segment %d: senc has %d entries, trun has %d samples", segN, len(ps.samples), len(cipherSamples))
						}
						for i, ct := range cipherSamples {
							got := decryptSample(t, scheme, cencKey, r.isVideo, ct, ps.samples[i], cenc.IV)
							if !bytes.Equal(got, plainSamples[i]) {
								t.Fatalf("segment %d sample %d: decrypted != clear (%d vs %d bytes)", segN, i, len(got), len(plainSamples[i]))
							}
						}

						// saio must point exactly at senc's first aux-info byte.
						if len(ps.samples) > 0 {
							want := ps.sencIVAbsOff - ps.moofStart
							if ps.saioOffset != want {
								t.Errorf("segment %d: saio = %d, want %d (senc first IV byte from moof start)", segN, ps.saioOffset, want)
							}
						}

						// Clear-region readability (video only): the NAL length
						// fields and headers must be parseable straight out of
						// the CIPHERTEXT mdat, chaining exactly to the sample end,
						// and matching senc's own recorded subsample split.
						if r.isVideo {
							for i, ct := range cipherSamples {
								subs, err := splitNALSubsamples(ct, 1)
								if err != nil {
									t.Fatalf("segment %d sample %d: NAL lengths unreadable in ciphertext: %v", segN, i, err)
								}
								if len(subs) != len(ps.samples[i].subs) {
									t.Fatalf("segment %d sample %d: %d NALs parsed from ciphertext, senc recorded %d", segN, i, len(subs), len(ps.samples[i].subs))
								}
								for k, sub := range subs {
									if int(sub.clear) != ps.samples[i].subs[k][0] || int(sub.protected) != ps.samples[i].subs[k][1] {
										t.Fatalf("segment %d sample %d subsample %d: parsed {%d,%d}, senc {%d,%d}",
											segN, i, k, sub.clear, sub.protected, ps.samples[i].subs[k][0], ps.samples[i].subs[k][1])
									}
								}
							}

							// cbcs pattern, first sample's big NAL (300-byte
							// protected region): block 0 must differ from clear
							// (encrypted), blocks 1-9 must equal clear (skipped).
							if scheme == "cbcs" && segN == 1 {
								sub := ps.samples[0].subs[1] // NAL2
								clearStart := 8 + sub[0]     // NAL1 (8 bytes) + NAL2's clear prefix
								protStart := clearStart
								ctProt := cipherSamples[0][protStart : protStart+sub[1]]
								ptProt := plainSamples[0][protStart : protStart+sub[1]]
								if bytes.Equal(ctProt[:16], ptProt[:16]) {
									t.Error("cbcs: block 0 of the protected region was not encrypted")
								}
								for b := 1; b <= 9; b++ {
									off := b * 16
									if !bytes.Equal(ctProt[off:off+16], ptProt[off:off+16]) {
										t.Errorf("cbcs: block %d of the protected region should be clear (skip), differs from plaintext", b)
									}
								}
							}
						}
					}
				})
			}
		})
	}
}

// seg1To renames a segNNNNN.m4s / seg_a1_NNNNN.m4s first-segment name to its
// n-th (1-based) sibling.
func seg1To(name string, n int) string {
	switch name {
	case "seg00001.m4s":
		return fmt.Sprintf("seg%05d.m4s", n)
	case "seg_a1_00001.m4s":
		return fmt.Sprintf("seg_a1_%05d.m4s", n)
	default:
		return name
	}
}

// The sacred invariant: a CENC-protected on-demand plan must produce
// byte-identical resources to the full pass - which is what proves the IV
// derivation (dtsTS-based, see cenc.go) is truly segment-independent.
func TestCENCBytePartity(t *testing.T) {
	for _, scheme := range []string{"cenc", "cbcs"} {
		t.Run(scheme, func(t *testing.T) {
			src := buildCENCFixture(t)
			cenc := &CENCOptions{Scheme: scheme, Key: cencKey, KeyID: cencKID, IV: cencIVFor(scheme), KeyURI: "https://example.test/key"}
			opts := Options{SegmentMs: 1000, CENC: cenc}

			fullDir := t.TempDir()
			if err := RemuxToHLS(context.Background(), src, fullDir, opts); err != nil {
				t.Fatal(err)
			}
			plan, err := PlanHLS(context.Background(), src, opts)
			if err != nil {
				t.Fatal(err)
			}

			// master.m3u8/manifest.mpd are NOT expected to be byte-identical for
			// a Matroska source (BANDWIDTH is estimated from cue cluster offsets,
			// see hlsplan.go's PlanHLS doc comment) - every packaged media
			// resource is, which is the sacred invariant this test proves.
			names := []string{"init.mp4", "init_a1.mp4",
				"playlist.m3u8", "audio1.m3u8",
				"seg00001.m4s", "seg00002.m4s", "seg00003.m4s", "seg00004.m4s",
				"seg_a1_00001.m4s", "seg_a1_00002.m4s", "seg_a1_00003.m4s", "seg_a1_00004.m4s"}
			for _, name := range names {
				want, err := os.ReadFile(filepath.Join(fullDir, name))
				if err != nil {
					t.Fatalf("%s: full pass: %v", name, err)
				}
				got, _, err := plan.Resource(context.Background(), name)
				if err != nil {
					t.Fatalf("%s: plan: %v", name, err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s: plan differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
				}
			}
		})
	}
}

// HLS EXT-X-KEY and the DASH ContentProtection element must advertise the
// configured scheme/method and key ID exactly.
func TestCENCPlaylistSignaling(t *testing.T) {
	cases := []struct {
		scheme, method string
	}{
		{"cenc", "SAMPLE-AES-CTR"},
		{"cbcs", "SAMPLE-AES"},
	}
	for _, c := range cases {
		t.Run(c.scheme, func(t *testing.T) {
			src := buildCENCFixture(t)
			cenc := &CENCOptions{Scheme: c.scheme, Key: cencKey, KeyID: cencKID, IV: cencIVFor(c.scheme), KeyURI: "https://example.test/key"}
			dir := t.TempDir()
			if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, CENC: cenc}); err != nil {
				t.Fatal(err)
			}
			pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
			if err != nil {
				t.Fatal(err)
			}
			wantKey := fmt.Sprintf("#EXT-X-KEY:METHOD=%s,URI=\"https://example.test/key\",KEYFORMAT=\"identity\",KEYFORMATVERSIONS=\"1\"", c.method)
			if !bytes.Contains(pl, []byte(wantKey)) {
				t.Errorf("playlist.m3u8 missing %q:\n%s", wantKey, pl)
			}
			audioPl, err := os.ReadFile(filepath.Join(dir, "audio1.m3u8"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(audioPl, []byte(wantKey)) {
				t.Errorf("audio1.m3u8 missing the EXT-X-KEY line:\n%s", audioPl)
			}

			mpd, err := os.ReadFile(filepath.Join(dir, "manifest.mpd"))
			if err != nil {
				t.Fatal(err) // CENC (unlike Encrypt) still gets a DASH manifest
			}
			if !bytes.Contains(mpd, []byte(`xmlns:cenc="urn:mpeg:cenc:2013"`)) {
				t.Errorf("manifest.mpd missing the cenc namespace:\n%s", mpd)
			}
			wantCP := fmt.Sprintf(`<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="%s" cenc:default_KID="4b455949-442d-3031-3233-343536373839"/>`, c.scheme)
			if !bytes.Contains(mpd, []byte(wantCP)) {
				t.Errorf("manifest.mpd missing %q:\n%s", wantCP, mpd)
			}
		})
	}
}

// Refusal matrix: every combination cenc.go documents as an error.
func TestCENCRefusalMatrix(t *testing.T) {
	src := buildCENCFixture(t)
	good := func() *CENCOptions {
		return &CENCOptions{Scheme: "cenc", Key: append([]byte(nil), cencKey...), KeyID: append([]byte(nil), cencKID...),
			IV: append([]byte(nil), cencIV8...), KeyURI: "k"}
	}

	cases := []struct {
		name string
		opts Options
	}{
		{"encrypt+cenc", Options{CENC: good(), Encrypt: &HLSEncryption{Key: cencKey, KeyURI: "k"}}},
		{"singlefile+cenc", Options{CENC: good(), SingleFile: true}},
		{"bad key length", Options{CENC: &CENCOptions{Scheme: "cenc", Key: []byte("short"), KeyID: cencKID, IV: cencIV8, KeyURI: "k"}}},
		{"bad kid length", Options{CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: []byte("short"), IV: cencIV8, KeyURI: "k"}}},
		{"bad cenc iv length", Options{CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: []byte{1, 2, 3}, KeyURI: "k"}}},
		{"bad cbcs iv length", Options{CENC: &CENCOptions{Scheme: "cbcs", Key: cencKey, KeyID: cencKID, IV: cencIV8, KeyURI: "k"}}},
		{"unknown scheme", Options{CENC: &CENCOptions{Scheme: "cbc1", Key: cencKey, KeyID: cencKID, IV: cencIV16, KeyURI: "k"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := RemuxToHLS(context.Background(), src, t.TempDir(), c.opts); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}

	// AV1 video refused for CENC in this version (subsample rules differ).
	t.Run("av1 video refused", func(t *testing.T) {
		w, h := uint32(64), uint32(48)
		blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{0x12, 0x34}}}
		av1src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "av1", CodecPrivate: fakeAV1C, Width: &w, Height: &h}}, blocks)
		if err := RemuxToHLS(context.Background(), av1src, t.TempDir(), Options{CENC: good()}); err == nil {
			t.Error("expected AV1+CENC to be refused")
		}
	})
}

// TestCENCSampleIVCounterRangesDisjoint is a confidentiality guard, not a
// round-trip check: a decrypt==original test can never catch AES-CTR keystream
// reuse, because each sample decrypts correctly under its own IV. Reuse only
// shows up as two DIFFERENT samples sharing keystream. The 'cenc' per-sample IV
// must therefore vary only the high 8 bytes of the 16-byte counter block: the
// low 8 bytes are the per-block counter a single sample advances, so two
// samples whose starting counters differ only in those low bytes (by less than
// one sample's block count) would overlap. This test asserts the derived
// counter ranges are pairwise disjoint even with a dtsTS stride of 1 and large
// samples - the exact condition the earlier "add dtsTS to the whole IV" code
// violated.
func TestCENCSampleIVCounterRangesDisjoint(t *testing.T) {
	base := cencIV16             // 16-byte per-sample IV
	const blocksPerSample = 4096 // a ~64 KB sample: 4096 AES blocks
	type rng struct{ lo, hi [16]byte }
	as128 := func(b []byte) [16]byte { var a [16]byte; copy(a[:], b); return a }
	add := func(a [16]byte, n uint64) [16]byte {
		out := a
		addUint64BE(out[:], n)
		return out
	}
	less := func(x, y [16]byte) bool { return bytes.Compare(x[:], y[:]) < 0 }

	// dtsTS stride of 1: the worst case for the low-byte-corruption bug.
	var ranges []rng
	for dts := int64(0); dts < 200; dts++ {
		start := as128(cencCounterBlock(cencSampleIV(base, dts)))
		end := add(start, blocksPerSample) // exclusive upper counter bound
		ranges = append(ranges, rng{start, end})
	}
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			a, b := ranges[i], ranges[j]
			// Disjoint unless a.lo < b.hi AND b.lo < a.hi.
			if less(a.lo, b.hi) && less(b.lo, a.hi) {
				t.Fatalf("counter ranges for samples %d and %d overlap: keystream reuse (IV derivation bug)", i, j)
			}
		}
	}

	// The low 8 bytes (block-counter space) must be untouched by the per-sample
	// derivation; only the high 8 bytes carry dtsTS.
	for dts := int64(0); dts < 8; dts++ {
		iv := cencSampleIV(base, dts)
		if !bytes.Equal(iv[8:], base[8:]) {
			t.Fatalf("dts=%d: low 8 IV bytes changed (%x), must stay the block-counter space (%x)", dts, iv[8:], base[8:])
		}
	}
}
