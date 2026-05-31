package commands_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestOpenInput_StdinParityWithFile verifies that reading regfix.mkv via the
// streaming path (as openInput does when path=="-") yields the same track
// count, chapter count, and duration as the seekable file reader.
func TestOpenInput_StdinParityWithFile(t *testing.T) {
	const fixture = regfixMKV // reuse constant from reindex_test.go

	// Reference: seekable read from file.
	ref, err := reader.Open(context.Background(), fixture)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}

	// Stream: read raw bytes into a buffer, then parse via ReadStream.
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture bytes: %v", err)
	}
	got, _, err := reader.ReadStream(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}

	if len(got.Tracks) != len(ref.Tracks) {
		t.Errorf("tracks: got %d, want %d", len(got.Tracks), len(ref.Tracks))
	}
	if len(got.Chapters) != len(ref.Chapters) {
		t.Errorf("chapters: got %d, want %d", len(got.Chapters), len(ref.Chapters))
	}
	if got.DurationMs != ref.DurationMs {
		t.Errorf("duration_ms: got %d, want %d", got.DurationMs, ref.DurationMs)
	}
}
