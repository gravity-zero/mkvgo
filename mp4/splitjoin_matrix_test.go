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
			// Each part after the first opens, at its zero, on the keyframe at or after the cut it was asked for.
			for k, p := range parts[1:] {
				var want []byte
				for _, b := range c.Blocks()[matrix.VideoTrack] {
					if b.Keyframe && b.Timecode == c.ExpectedCuts()[k] {
						want = b.Data
					}
				}
				m := mustCutModel(t, p)
				v := &m.tracks[m.ref]
				f, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				s := v.tt.samples[0]
				if got := f[s.src : s.src+int64(s.size)]; !s.sync || v.ptNs(0, m.movieTS) != 0 || !bytes.Equal(got, want) {
					t.Errorf("%s opens on %x at %d ns (sync %v), want the keyframe at %d ms, %x, at 0", filepath.Base(p), got, v.ptNs(0, m.movieTS), s.sync, c.ExpectedCuts()[k], want)
				}
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
			// The samples' own times leave the edit list out: each track must also present first where the source's does.
			sm, jm := mustCutModel(t, src), mustCutModel(t, joined)
			for ti := range sm.tracks {
				if a, b := sm.tracks[ti].ptNs(0, sm.movieTS), jm.tracks[ti].ptNs(0, jm.movieTS); a != b {
					t.Errorf("track %d presents first at %d ns, the source at %d", ti+1, b, a)
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
			if w, g := chapterTrackCount(t, src), chapterTrackCount(t, joined); w != g {
				t.Errorf("the chapter track holds %d samples, the source's %d", g, w)
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

// TestSplitJoinSecondVideoMatrix: as in Matroska, a second video track out of step with the cuts comes back whole, each
// part opening it on one of its keyframes and lasting as its cut track does.
func TestSplitJoinSecondVideoMatrix(t *testing.T) {
	for _, c := range matrix.SecondVideoCases() {
		t.Run(c.Name(), func(t *testing.T) {
			dir := t.TempDir()
			mkvPath := filepath.Join(dir, "src.mkv")
			if err := c.BuildMKV(mkvPath); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(dir, "src.mp4")
			if err := RemuxToMP4(context.Background(), mkvPath, src); err != nil {
				t.Fatal(err)
			}
			parts := splitInto(t, src, mkv.SplitOptions{Ranges: c.Ranges()})
			twin := c
			twin.Second = false
			twinMKV, twinSrc := filepath.Join(dir, "twin.mkv"), filepath.Join(dir, "twin.mp4")
			if err := twin.BuildMKV(twinMKV); err != nil {
				t.Fatal(err)
			}
			if err := RemuxToMP4(context.Background(), twinMKV, twinSrc); err != nil {
				t.Fatal(err)
			}
			twinParts := splitInto(t, twinSrc, mkv.SplitOptions{Ranges: twin.Ranges()})
			cuts := c.ExpectedCuts()
			if len(parts) != len(cuts)+1 {
				t.Fatalf("%d parts, want %d", len(parts), len(cuts)+1)
			}
			for k, p := range parts {
				m := mustCutModel(t, p)
				second := -1
				for i := range m.tracks {
					if m.tracks[i].handler == "vide" && i != m.ref {
						second = i
					}
				}
				if second < 0 {
					t.Fatalf("%s has no second video track", filepath.Base(p))
				}
				if s := m.tracks[second].tt.samples; k > 0 && len(s) > 0 && !s[0].sync {
					t.Errorf("%s opens the second video off its keyframes", filepath.Base(p))
				}
				if tp := mustMeta(t, twinParts[k]); mustMeta(t, p).DurationMs < tp.DurationMs-1 {
					t.Errorf("%s lasts %d ms, less than the %d it lasts without the second video", filepath.Base(p), mustMeta(t, p).DurationMs, tp.DurationMs)
				}
			}
			joined := filepath.Join(dir, "j.mp4")
			if err := Join(context.Background(), parts, joined); err != nil {
				t.Fatal(err)
			}
			if s, j := mustMeta(t, src).DurationMs, mustMeta(t, joined).DurationMs; s-j < -1 || s-j > 1 {
				t.Errorf("the join lasts %d ms, the source %d", j, s)
			}
			want, got := sampleSeqs(t, src), sampleSeqs(t, joined)
			for ti := range want {
				if strings.Join(want[ti], ",") != strings.Join(got[ti], ",") {
					t.Errorf("track %d: the join does not give the source back", ti+1)
					diffFirst(t, want[ti], got[ti])
				}
			}
		})
	}
}
