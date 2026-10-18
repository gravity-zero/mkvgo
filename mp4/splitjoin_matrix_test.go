package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitJoinMatrix holds every matrix case, remuxed to MP4, to what split then join must keep: every sample, the chapters, the cue on screen at each cut, and no split link.
func TestSplitJoinMatrix(t *testing.T) {
	for _, c := range matrix.Cases() {
		t.Run(c.Name(), func(t *testing.T) {
			dir := t.TempDir()
			mkvPath := filepath.Join(dir, "src.mkv")
			if err := c.BuildMKV(mkvPath); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(dir, "src.mp4")
			if err := RemuxToMP4(context.Background(), mkvPath, src, Options{}); err != nil {
				t.Fatal(err)
			}
			parts := splitInto(t, src, mkv.SplitOptions{Ranges: c.Ranges()})
			if len(parts) != c.Cuts+1 {
				t.Fatalf("%d parts, want %d", len(parts), c.Cuts+1)
			}
			// The cue is on screen at the first cut, and at the second too when it runs over both.
			covered := map[matrix.Subtitle]int{matrix.CueOverCut: 1, matrix.CueOverTwoCuts: 2}[c.Subtitle]
			if covered > 0 {
				for _, p := range parts[1 : 1+covered] {
					m := mustCutModel(t, p)
					if tc := &m.tracks[textTrackIndex(t, m)]; len(tc.tt.samples) == 0 || tc.ptNs(0, m.movieTS) > 0 {
						t.Errorf("%s does not open on the cue on screen at its cut", filepath.Base(p))
					}
				}
			}
			joined := filepath.Join(dir, "j.mp4")
			if err := Join(context.Background(), parts, joined); err != nil {
				t.Fatal(err)
			}
			want, got := sampleSeqs(t, src), sampleSeqs(t, joined)
			if len(want) != len(got) {
				t.Fatalf("%d tracks joined, %d in the source", len(got), len(want))
			}
			for ti := range want {
				if strings.Join(want[ti], ",") != strings.Join(got[ti], ",") {
					t.Errorf("track %d: the join does not give the source back", ti+1)
					diffFirst(t, want[ti], got[ti])
				}
			}
			wc, gc := mustMeta(t, src).Chapters, mustMeta(t, joined).Chapters
			if len(wc) != len(gc) {
				t.Fatalf("chapters %+v, want %+v", gc, wc)
			}
			for i := range wc {
				if wc[i].Title != gc[i].Title || wc[i].StartMs != gc[i].StartMs || wc[i].EndMs != gc[i].EndMs {
					t.Errorf("chapter %d: %+v, want %+v", i+1, gc[i], wc[i])
				}
			}
			if tags := splitLinkTags(t, joined); len(tags) != 0 {
				t.Errorf("the join carries %v", tags)
			}
		})
	}
}
