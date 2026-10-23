package mp4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Fixed encryption vectors, checked without any outside tool. cryptvec/src.mkv
// is a synthetic source: 6 H.264 frames of 3 slices each (AVCC, so 3
// subsamples a frame, parameter sets in band on keyframes) and 12 AAC frames. Regenerate with
//
//	MKVGO_GEN_CRYPTVEC=1 go test ./mp4 -run TestGenerateCryptVectors
//	cd internal/testdata/cryptvec
//	openssl enc -aes-128-cbc -K 000102030405060708090a0b0c0d0e0f -iv 101112131415161718191a1b1c1d1e1f -in seg00001.plain -out seg00001.aes
//	cat init.cenc seg00001.cenc > /tmp/cenc.mp4 && ffmpeg -decryption_key 000102030405060708090a0b0c0d0e0f -i /tmp/cenc.mp4 -c copy -f streamhash -hash sha256 -format_version 1 -
//	cat init.cbcs seg00001.cbcs > /tmp/cbcs.mp4 && ffmpeg -decryption_key 000102030405060708090a0b0c0d0e0f -i /tmp/cbcs.mp4 -c copy -f streamhash -hash sha256 -format_version 1 -
//	ffmpeg -i src.mkv -map 0:v -c copy -f streamhash -hash sha256 -format_version 1 -
//
// then set cryptVecSums from sha256sum of init + segment and check that both
// decryptions' streamhash is the source's (cryptVecPlainVideo).
const cryptVecDir = "../internal/testdata/cryptvec"

var (
	cryptVecKey = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	cryptVecIV  = []byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f}
	cryptVecKID = []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf}

	// sha256 of init + seg00001 (video) as mkvgo encrypts them, and the
	// streamhash of their decryption by ffmpeg, equal to the source's video.
	cryptVecSums = map[string]string{
		"cenc": "9e2faa284476f762201264f9e523d7a854141382ba37c65ea927a79d3ac17c0d",
		"cbcs": "f36b1cdae872e3bed48daa88666990f598bfb398391727d0d901ee1f84f06204",
	}
	cryptVecPlainVideo = "119527985453b51e58d57c25185c7b69a8f2743bf6799006574bf9722d04b76d"
)

func cryptVecOptions(mode string) Options {
	o := Options{SegmentMs: 2000}
	switch mode {
	case "aes-128":
		o.Encrypt = &HLSEncryption{Key: cryptVecKey, IV: cryptVecIV, KeyURI: "key.bin"}
	case "cenc":
		o.CENC = &CENCOptions{Scheme: "cenc", Key: cryptVecKey, KeyID: cryptVecKID, IV: cryptVecIV[:8], InlineKey: true}
	case "cbcs":
		o.CENC = &CENCOptions{Scheme: "cbcs", Key: cryptVecKey, KeyID: cryptVecKID, IV: cryptVecIV, InlineKey: true}
	}
	return o
}

// cryptVecFrame is frame i: in-band SPS and PPS on a keyframe, then three
// slices of a few dozen bytes, every frame of a kind the same size.
func cryptVecFrame(i int, key bool) []byte {
	var nals [][]byte
	if key {
		nals = append(nals, testAVCSPS(), testAVCPPS())
	}
	for s := 0; s < 3; s++ {
		nal, _ := testAVCSlice(uint32(s*4), uint32(7+i%8), 40+13*s)
		nals = append(nals, nal)
	}
	return lengthPrefixed(4, nals...)
}

// TestGenerateCryptVectors writes cryptvec/src.mkv and mkvgo's segments; see cryptVecDir.
func TestGenerateCryptVectors(t *testing.T) {
	if os.Getenv("MKVGO_GEN_CRYPTVEC") == "" {
		t.Skip("set MKVGO_GEN_CRYPTVEC=1 to regenerate the vectors")
	}
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var blocks []genBlock
	for i := 0; i < 6; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 200, key: i%3 == 0, data: cryptVecFrame(i, i%3 == 0)})
		for a := 0; a < 2; a++ {
			blocks = append(blocks, genBlock{track: 2, pts: int64(i)*200 + int64(a)*100, key: true, data: bytes.Repeat([]byte{0x21, byte(i*2 + a)}, 30)})
		}
	}
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, blocks)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(cryptVecDir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src.mkv", data)
	ctx := context.Background()
	for mode, files := range map[string][2]string{"": {"", "seg00001.plain"}, "cenc": {"init.cenc", "seg00001.cenc"}, "cbcs": {"init.cbcs", "seg00001.cbcs"}} {
		plan := mustPlan(t, filepath.Join(cryptVecDir, "src.mkv"), cryptVecOptions(mode))
		seg, _, err := plan.Resource(ctx, "seg00001.m4s")
		if err != nil {
			t.Fatal(err)
		}
		write(files[1], seg)
		if files[0] != "" {
			init, _, err := plan.Resource(ctx, "init.mp4")
			if err != nil {
				t.Fatal(err)
			}
			write(files[0], init)
		}
	}
}

// Open and Resource reproduce the fixed vectors exactly: the AES-128 segment
// openssl encrypted from mkvgo's plaintext, and the cenc and cbcs segments
// ffmpeg decrypted to the source's packets.
func TestCryptVectors(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(cryptVecDir, "src.mkv")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(cryptVecDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	both := func(plan *HLSPlan, name string) []byte {
		want, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		h, err := plan.Open(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if _, err := h.WriteTo(ctx, &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("%s: Open differs from Resource", name)
		}
		return want
	}
	plain := both(mustPlan(t, src, cryptVecOptions("")), "seg00001.m4s")
	if !bytes.Equal(plain, read("seg00001.plain")) {
		t.Fatal("the plaintext segment is not the one the vectors were made from")
	}
	if got := both(mustPlan(t, src, cryptVecOptions("aes-128")), "seg00001.m4s"); !bytes.Equal(got, read("seg00001.aes")) {
		t.Error("aes-128: the segment is not openssl's encryption of the plaintext")
	}
	for _, mode := range []string{"cenc", "cbcs"} {
		plan := mustPlan(t, src, cryptVecOptions(mode))
		init, _, err := plan.Resource(ctx, "init.mp4")
		if err != nil {
			t.Fatal(err)
		}
		file := append(init, both(plan, "seg00001.m4s")...)
		sum := sha256.Sum256(file)
		if got := hex.EncodeToString(sum[:]); got != cryptVecSums[mode] {
			t.Errorf("%s: init + segment sha256 %s, want %s", mode, got, cryptVecSums[mode])
		}
		if ffmpeg := os.Getenv("MKVGO_FFMPEG"); ffmpeg != "" { // the vectors' decryption, again
			f := filepath.Join(t.TempDir(), mode+".mp4")
			if err := os.WriteFile(f, file, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := streamHashes(t, ffmpeg, "-decryption_key", hex.EncodeToString(cryptVecKey), "-i", f, "-map", "0:v")["v"]; got != "SHA256="+cryptVecPlainVideo {
				t.Errorf("%s: ffmpeg decrypts the video to %s, the source is %s", mode, got, cryptVecPlainVideo)
			}
		}
	}
}
