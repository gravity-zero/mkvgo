package mp4

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestJoinAcrossTwoSplits: parts of two splits of one source at other cuts, or out of order, or one part twice, join as unrelated files: end to end, every chapter and cue kept, nothing merged at a seam that is no cut.
func TestJoinAcrossTwoSplits(t *testing.T) {
	for _, name := range []string{"bframes_aac.mp4", "bframes_aac.mov", "negative_cts.mp4", "hevc_opengop.mp4", "fps30.mp4"} {
		src := copyFixture(t, name)
		kf := mustCutModel(t, src).keyframesMs()
		if len(kf) < 3 {
			t.Fatalf("%s: %d keyframes", name, len(kf))
		}
		src = dressed(t, src, marked, kf[1:3]) // chapters 300 ms ahead of both cuts, a cue over the first
		a := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: kf[1]}, {StartMs: kf[1]}}})
		b := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: kf[2]}, {StartMs: kf[2]}}})
		for _, c := range []struct {
			name  string
			files []string
		}{
			{"hole", []string{a[0], b[1]}},
			{"overlap", []string{b[0], a[1]}},
			{"reversed", []string{a[1], a[0]}},
			{"twice", []string{a[1], a[1]}},
		} {
			t.Run(name+"/"+c.name, func(t *testing.T) {
				dst := filepath.Join(t.TempDir(), "j"+filepath.Ext(name))
				err := Join(context.Background(), c.files, dst)
				// A part opening on an open GOP decodes further ahead than one opening the film: after it, its leading pictures find no room on the decode timeline.
				if name == "hevc_opengop.mp4" && (c.name == "overlap" || c.name == "twice") {
					// The refusal names the file that decodes ahead and the route through MKV, not a tail the previous file does not hide.
					if err == nil || !strings.Contains(err.Error(), "file 2 starts decoding") || !strings.Contains(err.Error(), "before file 1 ends") || strings.Contains(err.Error(), "hides") || !strings.Contains(err.Error(), "from-mp4") {
						t.Errorf("an open-GOP part after a shallower file: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				checkJoinedEndToEndCues(t, c.files, dst)
				// Independently: no picture of the second file presents ahead of its first keyframe, the references it would decode from are not there.
				jc, first := mustCutModel(t, dst), mustCutModel(t, c.files[0])
				jv, n := &jc.tracks[jc.ref], len(first.tracks[first.ref].tt.samples)
				for i := n + 1; i < len(jv.tt.samples); i++ {
					if s, key := jv.tt.samples[i], jv.tt.samples[n]; !s.sync && s.dts+int64(s.cto) < key.dts+int64(key.cto) {
						t.Fatalf("picture %d presents ahead of the keyframe the second file opens on", i+1)
					}
					if jv.tt.samples[i].sync {
						break
					}
				}
				var want []mkv.Chapter
				var total int64
				for _, f := range c.files {
					m := mustMeta(t, f)
					for _, ch := range m.Chapters {
						ch.StartMs += total
						want = append(want, ch)
					}
					total += m.DurationMs
				}
				jm := mustMeta(t, dst)
				if absMs(jm.DurationMs-total) > 1 {
					t.Errorf("the join lasts %d ms, its files %d", jm.DurationMs, total)
				}
				if len(jm.Chapters) != len(want) {
					t.Fatalf("chapters %+v, want each file's %+v", jm.Chapters, want)
				}
				for i, w := range want {
					if g := jm.Chapters[i]; g.Title != w.Title || absMs(g.StartMs-w.StartMs) > 1 {
						t.Errorf("chapter %d: %+v, want %+v", i+1, g, w)
					}
				}
				if n := chapterTrackCount(t, dst); n != len(want) {
					t.Errorf("the chapter track holds %d samples, the chapters %d", n, len(want))
				}
				if tags := splitLinkTags(t, dst); len(tags) != 0 {
					t.Errorf("the join carries %v", tags)
				}
			})
		}
	}
}

// checkJoinedEndToEndCues is checkJoinedEndToEnd with the empty samples of text tracks set aside: a join fills a subtitle gap with one, as a source holds one between cues.
func checkJoinedEndToEndCues(t *testing.T, files []string, dst string) {
	t.Helper()
	empty := fmt.Sprintf("true/%x", sha256.Sum256([]byte{0, 0}))
	jm := mustCutModel(t, dst)
	text := map[int]bool{}
	for ti := range jm.tracks {
		text[ti] = jm.tracks[ti].isText()
	}
	cues := func(ti int, seq []string) []string {
		if !text[ti] {
			return seq
		}
		var out []string
		for _, s := range seq {
			if s != empty {
				out = append(out, s)
			}
		}
		return out
	}
	got := sampleSeqs(t, dst)
	want := make([][]string, len(got))
	var prev *cutModel
	for k, f := range files {
		seqs := sampleSeqs(t, f)
		if len(seqs) != len(got) {
			t.Fatalf("%s has %d tracks, the join %d", filepath.Base(f), len(seqs), len(got))
		}
		m := mustCutModel(t, f)
		for ti, seq := range seqs {
			from, to := shownAtSeam(&m.tracks[ti], m.movieTS, k > 0, k < len(files)-1)
			want[ti] = append(want[ti], cues(ti, keptAtSeam(&m.tracks[ti], bodies(seq), from, to, unlinkedAfter(m, prev)))...)
		}
		prev = m
	}
	for ti := range got {
		if g := cues(ti, bodies(got[ti])); strings.Join(g, ",") != strings.Join(want[ti], ",") {
			t.Errorf("track %d: the join does not hold its files end to end (%d samples, want %d)", ti+1, len(g), len(want[ti]))
			diffFirst(t, want[ti], g)
		}
	}
}
