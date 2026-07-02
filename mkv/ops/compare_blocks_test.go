package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A reindex is a verbatim cluster copy: CompareBlocks must report zero diffs.
// A remux through RemuxToWebM copies payloads verbatim too. A file missing one
// block must be reported as a content diff.
func TestCompareBlocks(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	for tc := int64(0); tc <= 3000; tc += 250 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: tc%1000 == 0,
			Data: []byte{byte(tc / 250), 0xAB}})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 3200)

	reindexed := filepath.Join(dir, "re.mkv")
	if err := Reindex(context.Background(), src, reindexed); err != nil {
		t.Fatal(err)
	}
	diffs, err := CompareBlocks(context.Background(), src, reindexed)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Errorf("reindex round-trip content diffs = %+v, want none", diffs)
	}

	// Drop one block → the content compare must flag it.
	truncated := buildMinimalMKV(t, dir, "trunc.mkv", []mkv.Track{videoTrack(1)}, blocks[:len(blocks)-1], 3200)
	diffs, err = CompareBlocks(context.Background(), src, truncated)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Section != "track[1].blocks" {
		t.Errorf("diffs = %+v, want one track[1].blocks change", diffs)
	}

	// Same count and size but different payload bytes → content hash differs.
	altered := make([]mkv.Block, len(blocks))
	copy(altered, blocks)
	altered[3].Data = []byte{0xFF, 0xAB}
	tampered := buildMinimalMKV(t, dir, "tamper.mkv", []mkv.Track{videoTrack(1)}, altered, 3200)
	diffs, err = CompareBlocks(context.Background(), src, tampered)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Section != "track[1].content" {
		t.Errorf("diffs = %+v, want one track[1].content change", diffs)
	}
}
