package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestStrictRefusesConcatDrops: a concatenation whose parts' subtitle layouts differ drops the subtitles with a
// reason by default; Options.Strict refuses it by the track's name, written and planned alike.
func TestStrictRefusesConcatDrops(t *testing.T) {
	ctx := context.Background()
	src0 := buildConcatSource(t, 2000, "eng", "a", 100)
	src1 := buildConcatSourceNoSubs(t, 1000)
	err := RemuxConcatToHLS(ctx, []string{src0, src1}, filepath.Join(t.TempDir(), "hls"), Options{SegmentMs: 500, Strict: true})
	if err == nil || !strings.Contains(err.Error(), "subtitle rendition layout differs") || !strings.Contains(err.Error(), "Options.Strict") {
		t.Errorf("concat strict: err = %v, want the subtitle track refused by Options.Strict", err)
	}
	if _, err := PlanConcat(ctx, []string{src0, src1}, Options{SegmentMs: 500, Strict: true}); err == nil || !strings.Contains(err.Error(), "Options.Strict") {
		t.Errorf("plan strict: err = %v, want the subtitle track refused by Options.Strict", err)
	}
}
