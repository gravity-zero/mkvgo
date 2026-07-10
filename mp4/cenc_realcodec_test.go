package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Real AV1 and VP9 bitstreams (tiny, synthetic - built from a test pattern, no
// real content) exercise the actual codec parsers end to end: the AV1
// frame_header_obu() parse over combined OBU_FRAME samples, and the VP9
// stateful splitter resolving inter frames' reference-frame width from the
// segment keyframe. Encrypting them proves the parsers handle real encoder
// output, not just hand-built fixtures - the same streams are validated
// decrypting and decoding in a Clear Key player out of band.
var realCodecFixtures = []struct {
	name  string
	path  string
	codec string
}{
	{"av1", "../internal/testdata/av1.mkv", "av1"},
	{"vp9", "../internal/testdata/vp9.webm", "vp9"},
}

func TestCENCRealCodecBitstreams(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef")
	kid := []byte("KID-sixteen-byte")
	for _, f := range realCodecFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			if _, err := os.Stat(f.path); err != nil {
				t.Skipf("fixture %s missing", f.path)
			}
			// Clear pass for a byte reference, then cenc and cbcs.
			clearDir := t.TempDir()
			if err := RemuxToHLS(ctx, f.path, clearDir, Options{SegmentMs: 1000}); err != nil {
				t.Fatalf("clear: %v", err)
			}
			clearSeg, _ := os.ReadFile(filepath.Join(clearDir, "seg00001.m4s"))

			for _, scheme := range []string{"cenc", "cbcs"} {
				iv := make([]byte, 16)
				if scheme == "cenc" {
					iv = iv[:8]
				}
				encDir := t.TempDir()
				err := RemuxToHLS(ctx, f.path, encDir, Options{SegmentMs: 1000,
					CENC: &CENCOptions{Scheme: scheme, Key: key, KeyID: kid, IV: iv}})
				if err != nil {
					t.Fatalf("%s: the real %s parser rejected the bitstream: %v", scheme, f.codec, err)
				}
				// The DASH manifest carries a ContentProtection element.
				mpd, err := os.ReadFile(filepath.Join(encDir, "manifest.mpd"))
				if err != nil || !bytes.Contains(mpd, []byte("ContentProtection")) {
					t.Errorf("%s: manifest.mpd missing ContentProtection", scheme)
				}
				// The encrypted segment must differ from the clear one (something
				// was actually protected - the parser found tile data).
				encSeg, _ := os.ReadFile(filepath.Join(encDir, "seg00001.m4s"))
				if len(clearSeg) > 0 && bytes.Equal(encSeg, clearSeg) {
					t.Errorf("%s: encrypted segment is identical to the clear one (nothing protected)", scheme)
				}
				// Plan and full pass agree byte for byte.
				plan, err := PlanHLS(ctx, f.path, Options{SegmentMs: 1000,
					CENC: &CENCOptions{Scheme: scheme, Key: key, KeyID: kid, IV: iv}})
				if err != nil {
					t.Fatalf("%s plan: %v", scheme, err)
				}
				planSeg, err := plan.Segment(ctx, 0)
				if err != nil {
					t.Fatalf("%s plan segment: %v", scheme, err)
				}
				if !bytes.Equal(planSeg, encSeg) {
					t.Errorf("%s: plan ciphertext differs from the full pass", scheme)
				}
			}
		})
	}
}

// TestVP9CodecStringRealBitstream covers the vp9 RFC 6381 codec-string path on a
// real VP9 sample with no CodecPrivate (the case that made VP9 unplayable): the
// record is derived from the first frame and the level from the picture size.
func TestVP9CodecStringRealBitstream(t *testing.T) {
	path := "../internal/testdata/vp9.webm"
	if _, err := os.Stat(path); err != nil {
		t.Skip("vp9 fixture missing")
	}
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), path, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	master, _ := os.ReadFile(filepath.Join(dir, "master.m3u8"))
	if !bytes.Contains(master, []byte(`CODECS="vp09.`)) {
		t.Errorf("master playlist missing a valid vp09 codec string:\n%s", master)
	}
	// The level must not be 00 (a player rejects it).
	if bytes.Contains(master, []byte("vp09.00.00.")) {
		t.Errorf("vp9 codec string has an invalid level 00:\n%s", master)
	}
}
