package mp4

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// AES-128 HLS: the playlists carry EXT-X-KEY and rewritten URIs, the segments
// are whole-file CBC ciphertext that decrypts back to the clear segments, the
// DASH manifest is withheld, and the on-demand plan produces the identical
// ciphertext (the IV is the segment sequence, so both modes agree).
func TestHLSEncryptionAndRewrite(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 100; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 200; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
			data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		gblocks)

	key := []byte("0123456789abcdef")
	enc := &HLSEncryption{Key: key, KeyURI: "key.bin"}
	rewrite := func(name string) string { return name + "?tok=SIG" }

	clearDir, encDir := t.TempDir(), t.TempDir()
	if err := RemuxToHLS(context.Background(), src, clearDir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(context.Background(), src, encDir,
		Options{SegmentMs: 2000, Encrypt: enc, RewriteURL: rewrite}); err != nil {
		t.Fatal(err)
	}

	pl, err := os.ReadFile(filepath.Join(encDir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`#EXT-X-KEY:METHOD=AES-128,URI="key.bin"`,
		`#EXT-X-MAP:URI="init.mp4?tok=SIG"`, "seg00001.m4s?tok=SIG"} {
		if !bytes.Contains(pl, []byte(want)) {
			t.Errorf("encrypted playlist missing %q:\n%s", want, pl)
		}
	}
	if _, err := os.Stat(filepath.Join(encDir, "manifest.mpd")); !os.IsNotExist(err) {
		t.Error("manifest.mpd must not be written for an encrypted presentation")
	}
	// The init stays clear; the master and MPD URIs are rewritten too.
	initSeg, _ := os.ReadFile(filepath.Join(encDir, "init.mp4"))
	if !bytes.Contains(initSeg[:32], []byte("ftyp")) {
		t.Error("init segment must stay clear")
	}
	master, _ := os.ReadFile(filepath.Join(encDir, "master.m3u8"))
	if !bytes.Contains(master, []byte("playlist.m3u8?tok=SIG")) {
		t.Errorf("master URIs not rewritten:\n%s", master)
	}

	// Segment 1: ciphertext, decrypts to the clear run's segment (IV = seq 0).
	ct, err := os.ReadFile(filepath.Join(encDir, "seg00001.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct[:16], []byte("styp")) || len(ct)%aes.BlockSize != 0 {
		t.Fatalf("segment does not look encrypted (%d bytes)", len(ct))
	}
	clear1, err := os.ReadFile(filepath.Join(clearDir, "seg00001.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, enc.segmentIV(0)).CryptBlocks(pt, ct)
	pt = pt[:len(pt)-int(pt[len(pt)-1])] // strip PKCS#7
	if !bytes.Equal(pt, clear1) {
		t.Fatalf("decrypted segment differs from the clear run (%d vs %d bytes)", len(pt), len(clear1))
	}

	// The on-demand plan emits the same ciphertext.
	plan, err := PlanHLS(context.Background(), src,
		Options{SegmentMs: 2000, Encrypt: enc, RewriteURL: rewrite})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := plan.Resource(context.Background(), "seg00001.m4s")
	if err != nil || !bytes.Equal(got, ct) {
		t.Errorf("plan ciphertext differs from the full pass (%v)", err)
	}
	if _, _, err := plan.Resource(context.Background(), "manifest.mpd"); err == nil {
		t.Error("encrypted plan must refuse manifest.mpd")
	}

	// Bad configurations fail loudly.
	if err := RemuxToHLS(context.Background(), src, t.TempDir(),
		Options{Encrypt: &HLSEncryption{Key: []byte("short"), KeyURI: "k"}}); err == nil {
		t.Error("short key must be rejected")
	}
	if err := RemuxToHLS(context.Background(), src, t.TempDir(),
		Options{Encrypt: &HLSEncryption{Key: key}}); err == nil {
		t.Error("missing KeyURI must be rejected")
	}
}
