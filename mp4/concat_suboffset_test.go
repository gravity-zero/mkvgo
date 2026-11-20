package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func concatVTT(t *testing.T, offset int64) []byte {
	t.Helper()
	srcs := []string{buildConcatSource(t, 2000, "eng", "a", 100), buildConcatSource(t, 2000, "eng", "b", 100)}
	o := Options{SegmentMs: 500, SubtitleOffsetMs: offset}
	var all bytes.Buffer
	dir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), srcs, dir, o); err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.vtt"))
	sort.Strings(names)
	for _, n := range names {
		b, _ := os.ReadFile(n)
		all.Write(b)
	}
	return all.Bytes()
}

// TestConcatAppliesSubtitleOffset: concat-hls lists --sub-offset; the cues it writes must move with it.
func TestConcatAppliesSubtitleOffset(t *testing.T) {
	base, shifted := concatVTT(t, 0), concatVTT(t, 300)
	if len(base) == 0 {
		t.Fatal("fixture: no WebVTT written")
	}
	if bytes.Equal(base, shifted) {
		t.Errorf("a 300 ms subtitle offset changed nothing in the WebVTT of a full-pass concat:\n%s", base)
	}
}

// The on-demand concat serves the same cues as the full pass, offset included.
func TestConcatPlanAppliesSubtitleOffset(t *testing.T) {
	srcs := []string{buildConcatSource(t, 2000, "eng", "a", 100), buildConcatSource(t, 2000, "eng", "b", 100)}
	o := Options{SegmentMs: 500, SubtitleOffsetMs: 300}
	dir := t.TempDir()
	if err := RemuxConcatToHLS(context.Background(), srcs, dir, o); err != nil {
		t.Fatal(err)
	}
	cp, err := PlanConcat(context.Background(), srcs, o)
	if err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.vtt"))
	more, _ := filepath.Glob(filepath.Join(dir, "p*", "*.vtt"))
	if len(names) == 0 || len(more) == 0 {
		t.Fatal("fixture: no WebVTT written")
	}
	for _, n := range append(names, more...) {
		want, _ := os.ReadFile(n)
		rel, _ := filepath.Rel(dir, n)
		got, _, err := cp.Resource(context.Background(), filepath.ToSlash(rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the plan serves\n%s\nthe full pass wrote\n%s", rel, got, want)
		}
	}
}
