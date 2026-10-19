package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDemuxNoOverwriteLeavesAnExistingFileAlone: a demux told not to overwrite refuses before writing, the file at an output name intact.
func TestDemuxNoOverwriteLeavesAnExistingFileAlone(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, testBlocks(1), 200)
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(out, "1.h264")
	if err := os.WriteFile(mine, []byte("not mkvgo's"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: out, NoOverwrite: true})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want a refusal naming the existing file", err)
	}
	if got, _ := os.ReadFile(mine); !bytes.Equal(got, []byte("not mkvgo's")) {
		t.Errorf("the existing file now holds %q", got)
	}
}
