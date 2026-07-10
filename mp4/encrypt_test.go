package mp4

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// buildEncSource writes a short H.264+AAC MKV with a keyframe every second, so
// SegmentMs=1000 cuts one segment per second - enough segments to see the key
// rotate.
func buildEncSource(t *testing.T) string {
	t.Helper()
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
	return buildMKV(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		gblocks)
}

// TestHLSKeyRotation: with RotateEverySegments=2 and two keys, the media
// playlist carries a fresh EXT-X-KEY at each period boundary (segments 0-1 use
// key A, 2-3 use key B, ...), each segment decrypts with its own period key,
// and the on-demand plan reproduces the same playlist and ciphertext byte for
// byte (the schedule is a pure function of the segment index).
func TestHLSKeyRotation(t *testing.T) {
	ctx := context.Background()
	src := buildEncSource(t)

	keyA, keyB := []byte("AAAAAAAAAAAAAAAA"), []byte("BBBBBBBBBBBBBBBB")
	enc := &HLSEncryption{
		RotateEverySegments: 2,
		Keys: []HLSKey{
			{Key: keyA, KeyURI: "https://k/a"},
			{Key: keyB, KeyURI: "https://k/b"},
		},
	}

	clearDir, encDir := t.TempDir(), t.TempDir()
	if err := RemuxToHLS(ctx, src, clearDir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToHLS(ctx, src, encDir, Options{SegmentMs: 1000, Encrypt: enc}); err != nil {
		t.Fatal(err)
	}

	pl, err := os.ReadFile(filepath.Join(encDir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	nSeg := bytes.Count(pl, []byte("#EXTINF"))
	if nSeg < 3 {
		t.Fatalf("need at least 3 segments to see rotation, got %d:\n%s", nSeg, pl)
	}
	// Both keys must appear, and the number of EXT-X-KEY lines equals the number
	// of period boundaries: ceil(nSeg / 2).
	if !bytes.Contains(pl, []byte(`URI="https://k/a"`)) || !bytes.Contains(pl, []byte(`URI="https://k/b"`)) {
		t.Errorf("playlist should carry both rotated key URIs:\n%s", pl)
	}
	gotKeys := bytes.Count(pl, []byte("#EXT-X-KEY"))
	wantKeys := (nSeg + 1) / 2
	if gotKeys != wantKeys {
		t.Errorf("EXT-X-KEY count = %d, want %d (one per rotation period over %d segments):\n%s", gotKeys, wantKeys, nSeg, pl)
	}
	// The first key line must be key A (segment 0), and it must sit after the MAP.
	mapIdx := bytes.Index(pl, []byte("#EXT-X-MAP"))
	firstKeyIdx := bytes.Index(pl, []byte("#EXT-X-KEY"))
	if mapIdx < 0 || firstKeyIdx < mapIdx {
		t.Errorf("first EXT-X-KEY must follow EXT-X-MAP (map %d, key %d)", mapIdx, firstKeyIdx)
	}

	// Each segment decrypts with its own period key back to the clear segment.
	for i := 0; i < nSeg; i++ {
		name := fmt.Sprintf("seg%05d.m4s", i+1)
		ct, err := os.ReadFile(filepath.Join(encDir, name))
		if err != nil {
			t.Fatal(err)
		}
		clear, err := os.ReadFile(filepath.Join(clearDir, name))
		if err != nil {
			t.Fatal(err)
		}
		k := enc.periodKey(i)
		wantKey := keyA
		if (i/2)%2 == 1 {
			wantKey = keyB
		}
		if !bytes.Equal(k.Key, wantKey) {
			t.Errorf("segment %d: period key mismatch", i)
		}
		block, _ := aes.NewCipher(k.Key)
		pt := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, segmentIV(k, uint32(i))).CryptBlocks(pt, ct)
		pt = pt[:len(pt)-int(pt[len(pt)-1])]
		if !bytes.Equal(pt, clear) {
			t.Errorf("segment %d did not decrypt to the clear run with its period key", i)
		}
	}

	// The on-demand plan reproduces the same playlist and ciphertext.
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 1000, Encrypt: enc})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plan.MediaPlaylist(), pl) {
		t.Errorf("plan media playlist differs from the full pass:\n--- plan ---\n%s\n--- full ---\n%s", plan.MediaPlaylist(), pl)
	}
	for i := 0; i < nSeg; i++ {
		seg, err := plan.Segment(ctx, i)
		if err != nil {
			t.Fatal(err)
		}
		ct, _ := os.ReadFile(filepath.Join(encDir, fmt.Sprintf("seg%05d.m4s", i+1)))
		if !bytes.Equal(seg, ct) {
			t.Errorf("segment %d: plan ciphertext differs from the full pass", i)
		}
	}
}

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
	cipher.NewCBCDecrypter(block, segmentIV(enc.periodKey(0), 0)).CryptBlocks(pt, ct)
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
