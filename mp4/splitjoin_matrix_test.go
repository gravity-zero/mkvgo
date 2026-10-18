package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/matrix"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestSplitJoinMatrix: every matrix case, as a hashed MP4, keeps through split then join its samples, chapters, cues at the cuts, groups and verifying hashes.
func TestSplitJoinMatrix(t *testing.T) {
	for _, c := range matrix.Cases() {
		t.Run(c.Name(), func(t *testing.T) {
			dir := t.TempDir()
			mkvPath := filepath.Join(dir, "src.mkv")
			if err := c.BuildMKV(mkvPath); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(dir, "src.mp4")
			if err := RemuxToMP4(context.Background(), mkvPath, src, Options{ContentHashes: true}); err != nil {
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
			for _, p := range parts {
				mustVerify(t, p)
			}
			if c.Audio {
				withUnknownMovieDuration(t, parts[0]) // a recorder's mvhd: the join takes the length from the tracks
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
			mustVerify(t, joined)
			sd, _ := os.ReadFile(src)
			jd, _ := os.ReadFile(joined)
			if ws, gs := trakGroups(t, sd), trakGroups(t, jd); fmt.Sprint(ws) != fmt.Sprint(gs) {
				t.Errorf("alternate groups %v, the source's %v", gs, ws)
			}
		})
	}
}

// withUnknownMovieDuration rewrites a file's mvhd duration (moov at the head or tail) to all ones, as a recorder still writing leaves it.
func withUnknownMovieDuration(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte("mvhd"))
	if i < 0 || data[i+4] != 0 {
		t.Fatal("no version 0 mvhd")
	}
	binary.BigEndian.PutUint32(data[i+4+16:], 0xFFFFFFFF)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
