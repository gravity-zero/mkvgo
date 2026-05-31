package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// regfixMKV is the real ffmpeg-muxed fixture used across the test suite.
const regfixMKV = "../../../internal/testdata/regfix.mkv"

// TestCmdReindex_ReindexRoundTrip verifies that the reindex operation used by
// CmdReindex produces a parseable output with a populated Cues index.
func TestCmdReindex_ReindexRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mkv")

	if err := ops.Reindex(context.Background(), regfixMKV, dst); err != nil {
		t.Fatalf("Reindex: %v", err)
	}

	// Output file must exist and be parseable.
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("output file missing: %v", err)
	}

	c, err := reader.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open reindexed output: %v", err)
	}

	// Cues index must be present and non-empty.
	if len(c.Cues) == 0 {
		t.Fatal("expected non-empty Cues in reindexed output")
	}
}
