package reader

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestReadISOBMFFTypedError covers an MP4-family file handed to the Matroska
// reader (a misnamed .mkv): instead of a cryptic EBML error, Read/ReadMeta return
// ErrNotMatroska so the caller can re-route to the mp4 reader.
func TestReadISOBMFFTypedError(t *testing.T) {
	mp4ish := []byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0}

	if _, err := Read(context.Background(), bytes.NewReader(mp4ish), "x.mkv"); !errors.Is(err, ErrNotMatroska) {
		t.Errorf("Read: want ErrNotMatroska, got %v", err)
	}
	if _, err := ReadMeta(context.Background(), bytes.NewReader(mp4ish), "x.mkv"); !errors.Is(err, ErrNotMatroska) {
		t.Errorf("ReadMeta: want ErrNotMatroska, got %v", err)
	}

	// Genuinely corrupt, non-ISOBMFF input keeps the plain EBML error.
	junk := []byte{0xAA, 0xBB, 0xCC, 0xDD, 'x', 'y', 'z', 'w'}
	if _, err := Read(context.Background(), bytes.NewReader(junk), "x.mkv"); errors.Is(err, ErrNotMatroska) {
		t.Error("non-ISOBMFF junk must not be flagged as ISOBMFF")
	}
}
