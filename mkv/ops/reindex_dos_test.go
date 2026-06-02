package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
)

// TestReindexRejectsHugeEBMLHeader is the exact attack the audit flagged: a
// ~6-byte file that declares a 10 GiB EBML header. Reindex must reject it via
// the size cap instead of make([]byte, 10GiB) (allocation bomb / OOM).
func TestReindexRejectsHugeEBMLHeader(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "evil.mkv")

	var buf bytes.Buffer
	ebml.WriteElementID(&buf, ebml.IDEBMLHeader)
	ebml.WriteDataSize(&buf, 10<<30) // declares 10 GiB; no body follows
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Reindex(context.Background(), src, filepath.Join(dir, "out.mkv"))
	if err == nil {
		t.Fatal("Reindex accepted a 10 GiB EBML header from a tiny file (would OOM)")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected the size-cap error, got: %v", err)
	}
}

// TestReindexRejectsHugeMetadataElement checks the metadata-buffer cap (reindex
// reads Info/Tracks/... bodies into memory): a tiny file declaring a huge Info
// element must be rejected, not allocated.
func TestReindexRejectsHugeMetadataElement(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "evil2.mkv")

	var seg bytes.Buffer
	// An Info element declaring 10 GiB, no body.
	ebml.WriteElementID(&seg, 0x1549A966) // IDInfo
	ebml.WriteDataSize(&seg, 10<<30)

	var buf bytes.Buffer
	ebml.WriteElementHeader(&buf, ebml.IDEBMLHeader, 0)
	ebml.WriteElementID(&buf, 0x18538067) // IDSegment
	ebml.WriteDataSize(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Reindex(context.Background(), src, filepath.Join(dir, "out2.mkv"))
	if err == nil {
		t.Fatal("Reindex accepted a 10 GiB Info element (would OOM)")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected the size-cap error, got: %v", err)
	}
}
