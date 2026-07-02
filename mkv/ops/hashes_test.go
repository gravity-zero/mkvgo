package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// hash → verify OK; flip one payload byte → mismatch reported; re-hash is
// idempotent (tags replaced, not stacked); unhashed file → explicit error.
func TestContentHashesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	marker := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	var blocks []mkv.Block
	for tc := int64(0); tc <= 2000; tc += 200 {
		data := append([]byte{byte(tc / 200)}, marker...)
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: data})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 2200)

	ctx := context.Background()
	if _, err := VerifyContentHashes(ctx, src); err == nil {
		t.Fatal("verify on an unhashed file must error")
	}

	// Hash to a new file, then re-hash IN PLACE (idempotence + reserve fit).
	hashed := filepath.Join(dir, "hashed.mkv")
	if err := WriteContentHashes(ctx, src, hashed); err != nil {
		t.Fatal(err)
	}
	if err := WriteContentHashes(ctx, hashed, ""); err != nil {
		t.Fatalf("in-place re-hash: %v", err)
	}

	mismatches, err := VerifyContentHashes(ctx, hashed)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 0 {
		t.Fatalf("pristine file reported mismatches: %+v", mismatches)
	}

	// Exactly one CONTENT_SHA256 tag after the double hash (replaced, not stacked).
	c, _, err := digestTracks(ctx, hashed, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, tag := range c.Tags {
		for _, st := range tag.SimpleTags {
			if st.Name == ContentHashTag {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("CONTENT_SHA256 tags = %d, want 1 (idempotent re-hash)", count)
	}

	// Flip one payload byte (bit rot) — verify must flag it.
	raw, err := os.ReadFile(hashed)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(raw, marker)
	if i < 0 {
		t.Fatal("marker payload not found in file")
	}
	raw[i] ^= 0xFF
	rotted := filepath.Join(dir, "rotted.mkv")
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
}
