package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// to-mp4 with ContentHashes makes the output self-verifying: verify passes on
// the pristine file, flags a corrupted sample, and errors on an unhashed file.
func TestMP4ContentHashesRoundTrip(t *testing.T) {
	marker := bytes.Repeat([]byte{0xCA, 0xFE}, 8)
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for tc := int64(0); tc <= 1000; tc += 100 {
		data := append([]byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(tc / 100)}, marker...)
		gblocks = append(gblocks, genBlock{track: 1, pts: tc, key: true, data: data})
	}
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: &w, Height: &h}},
		gblocks)

	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(ctx, src, out, Options{ContentHashes: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("CONTENT_SHA256_1")) || !bytes.Contains(raw, []byte("org.mkvgo")) {
		t.Fatal("hashed MP4 lacks the freeform CONTENT_SHA256 atom")
	}

	mismatches, err := VerifyContentHashes(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 0 {
		t.Fatalf("pristine MP4 reported mismatches: %+v", mismatches)
	}

	// Corrupt one sample byte inside the mdat - verify must flag track 1.
	i := bytes.Index(raw, marker)
	if i < 0 {
		t.Fatal("marker not found in mdat")
	}
	raw[i] ^= 0xFF
	rotted := filepath.Join(t.TempDir(), "rotted.mp4")
	if err := os.WriteFile(rotted, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	mismatches, err = VerifyContentHashes(ctx, rotted)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 1 || mismatches[0].TrackID != 1 {
		t.Errorf("mismatches = %+v, want one on track 1", mismatches)
	}

	// An MP4 without hashes must error explicitly.
	plain := filepath.Join(t.TempDir(), "plain.mp4")
	if err := RemuxToMP4(ctx, src, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyContentHashes(ctx, plain); err == nil {
		t.Error("verify on an unhashed MP4 must error")
	}
}

// FastStart layout must carry the hashes too (moov is built twice there).
func TestMP4ContentHashesFastStart(t *testing.T) {
	w, h := uint32(64), uint32(64)
	src := buildMKV(t,
		[]mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: &w, Height: &h}},
		[]genBlock{{track: 1, pts: 0, key: true, data: []byte{0x00, 0x00, 0x00, 0x01, 0x65}}})
	out := filepath.Join(t.TempDir(), "fs.mp4")
	if err := RemuxToMP4(context.Background(), src, out, Options{ContentHashes: true, FastStart: true}); err != nil {
		t.Fatal(err)
	}
	mismatches, err := VerifyContentHashes(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 0 {
		t.Errorf("faststart hashed MP4 reported mismatches: %+v", mismatches)
	}
}
