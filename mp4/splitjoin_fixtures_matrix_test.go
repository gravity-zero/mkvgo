package mp4

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/splitplan"
	"github.com/gravity-zero/mkvgo/mkv"
)

// fixtureCutSources are the real MP4/MOV fixtures with a keyframe to cut at: keyframes between milliseconds, B-frames, an open GOP, an audio delay in an edit list.
var fixtureCutSources = []string{"bframes_aac.mp4", "bframes_aac.mov", "fps30.mp4", "hevc_opengop.mp4", "interleaved.mp4", "negative_cts.mp4", "offset_ts48k.mp4", "slices.mp4"}

// dress is what a fixture case adds to its source before the split.
type dress int

const (
	bare    dress = iota
	marked        // open chapters and a cue over the first cut
	crowded       // ended chapters, a cue over the first cut, a second audio track and a second subtitle track
)

// TestSplitJoinFixtureMatrix: real fixtures, cut on their keyframes asked exactly or early and dressed with chapters, cues and extra tracks, come back from split then join.
func TestSplitJoinFixtureMatrix(t *testing.T) {
	for _, name := range fixtureCutSources {
		for _, early := range []bool{false, true} {
			for _, d := range []dress{bare, marked, crowded} {
				t.Run(fmt.Sprintf("%s/early=%v/dress=%d", name, early, d), func(t *testing.T) {
					src := copyFixture(t, name)
					kf := mustCutModel(t, src).keyframesMs()
					if len(kf) < 2 {
						t.Fatalf("%d keyframes", len(kf))
					}
					cuts := kf[1:min(len(kf), 3)]
					src = dressed(t, src, d, cuts)
					var ranges []mkv.TimeRange
					start := int64(0)
					for i, c := range cuts {
						ask := c
						if early {
							ask = (kf[i] + c) / 2 // between keyframes: the part still opens on c
						}
						ranges = append(ranges, mkv.TimeRange{StartMs: start, EndMs: ask})
						start = ask
					}
					ranges = append(ranges, mkv.TimeRange{StartMs: start})
					checkSplitJoin(t, src, mkv.SplitOptions{Ranges: ranges}, len(ranges), d != bare)
				})
			}
		}
	}
}

// TestSplitJoinFixtureModes: split every so often, or by the chapters a dress sets 300 ms ahead of each cut, comes back from the join too.
func TestSplitJoinFixtureModes(t *testing.T) {
	for _, name := range fixtureCutSources {
		t.Run(name+"/every", func(t *testing.T) {
			src := copyFixture(t, name)
			kf := mustCutModel(t, src).keyframesMs()
			every := kf[1]
			if len(kf) > 2 {
				every++ // past one GOP: parts of two
			}
			ranges, err := splitplan.EveryRanges(kf, every)
			if err != nil {
				t.Fatal(err)
			}
			parts := checkSplitJoin(t, src, mkv.SplitOptions{EveryMs: every}, len(ranges), false)
			for _, p := range parts[:len(parts)-1] {
				if d := mustMeta(t, p).DurationMs; d < every {
					t.Errorf("%s lasts %d ms, under the %d asked", filepath.Base(p), d, every)
				}
			}
		})
		for _, d := range []dress{marked, crowded} {
			t.Run(fmt.Sprintf("%s/chapters/dress=%d", name, d), func(t *testing.T) {
				src := copyFixture(t, name)
				kf := mustCutModel(t, src).keyframesMs()
				cuts := kf[1:min(len(kf), 3)]
				src = dressed(t, src, d, cuts)
				parts := checkSplitJoin(t, src, mkv.SplitOptions{ByChapters: true, Pattern: "{title}" + filepath.Ext(src)}, len(cuts)+1, true)
				for i, p := range parts {
					if want := fmt.Sprintf("Chapter %d%s", i+1, filepath.Ext(src)); filepath.Base(p) != want {
						t.Errorf("part %d named %q, want %q", i+1, filepath.Base(p), want)
					}
				}
			})
		}
	}
}

// TestSplitLeavingGaps: parts that skip the head or a stretch between two cuts each hold the source's samples from their keyframe on, and their join holds them end to end.
func TestSplitLeavingGaps(t *testing.T) {
	for _, name := range fixtureCutSources {
		src := copyFixture(t, name)
		kf := mustCutModel(t, src).keyframesMs()
		if len(kf) < 3 {
			continue
		}
		for _, ranges := range [][]mkv.TimeRange{
			{{StartMs: kf[1], EndMs: kf[2]}},
			{{StartMs: kf[1]}},
			{{StartMs: 0, EndMs: kf[1]}, {StartMs: kf[2]}},
		} {
			t.Run(fmt.Sprintf("%s/%v", name, ranges), func(t *testing.T) {
				parts := splitInto(t, src, mkv.SplitOptions{Ranges: ranges})
				whole := sampleSeqs(t, src)
				joined := make([][]string, len(whole))
				end, sum := mustMeta(t, src).DurationMs, int64(0)
				for i, p := range parts {
					want := cmp.Or(ranges[i].EndMs, end) - ranges[i].StartMs
					got := mustMeta(t, p).DurationMs
					if d := got - want; d < -1 || d > 1 {
						t.Errorf("%s lasts %d ms, its range %d", filepath.Base(p), got, want)
					}
					sum += got
					seqs := sampleSeqs(t, p)
					if len(seqs) != len(whole) {
						t.Fatalf("%s: %d tracks, %d in the source", filepath.Base(p), len(seqs), len(whole))
					}
					for ti, seq := range seqs {
						if !containsRun(bodies(whole[ti]), bodies(seq)) {
							t.Errorf("%s track %d: its samples are not a stretch of the source's", filepath.Base(p), ti+1)
						}
						joined[ti] = append(joined[ti], bodies(seq)...)
					}
				}
				if len(parts) < 2 {
					return
				}
				out := filepath.Join(t.TempDir(), "j"+filepath.Ext(src))
				if err := Join(context.Background(), parts, out); err != nil {
					t.Fatal(err)
				}
				for ti, seq := range sampleSeqs(t, out) {
					if strings.Join(bodies(seq), ",") != strings.Join(joined[ti], ",") {
						t.Errorf("track %d: the join does not hold its parts end to end", ti+1)
					}
				}
				if d := mustMeta(t, out).DurationMs - sum; d < -1 || d > 1 {
					t.Errorf("the join lasts %d ms, its parts %d", sum+d, sum)
				}
			})
		}
	}
}

// containsRun reports whether run appears in seq as consecutive entries.
func containsRun(seq, run []string) bool {
	return strings.Contains(","+strings.Join(seq, ",")+",", ","+strings.Join(run, ",")+",")
}

// TestSplitJoinAudioOnly: a source with no picture is cut at the asked times and joined back.
func TestSplitJoinAudioOnly(t *testing.T) {
	for _, name := range []string{"bframes_aac.mp4", "offset_ts48k.mp4"} {
		for _, d := range []dress{bare, marked} {
			t.Run(fmt.Sprintf("%s/dress=%d", name, d), func(t *testing.T) {
				src := filepath.Join(t.TempDir(), "audio.mp4")
				if err := RemoveTrack(context.Background(), realFixture(name), src, []uint64{1}); err != nil {
					t.Fatal(err)
				}
				cuts := []int64{700, 1300}
				src = dressed(t, src, d, cuts)
				checkSplitJoin(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: cuts[0]}, {StartMs: cuts[0], EndMs: cuts[1]}, {StartMs: cuts[1]}}}, 3, d != bare)
			})
		}
	}
}

// checkSplitJoin splits src into n parts, returned, and requires the join to give back its samples, chapters, chapter track, groups and length, with no link left; cued: the second part opens on a cue.
func checkSplitJoin(t *testing.T, src string, opts mkv.SplitOptions, n int, cued bool) []string {
	t.Helper()
	parts := splitInto(t, src, opts)
	if len(parts) != n {
		t.Fatalf("%d parts, want %d", len(parts), n)
	}
	if cued {
		m := mustCutModel(t, parts[1])
		if c := &m.tracks[textTrackIndex(t, m)]; len(c.tt.samples) == 0 || c.ptNs(0, m.movieTS) > 0 {
			t.Errorf("%s does not open on the cue on screen at its cut", filepath.Base(parts[1]))
		}
	}
	joined := filepath.Join(t.TempDir(), "j"+filepath.Ext(src))
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
	sm, jm := mustMeta(t, src), mustMeta(t, joined)
	if len(sm.Chapters) != len(jm.Chapters) {
		t.Fatalf("chapters %+v, want %+v", jm.Chapters, sm.Chapters)
	}
	for i := range sm.Chapters {
		if w, g := sm.Chapters[i], jm.Chapters[i]; w.Title != g.Title || w.StartMs != g.StartMs || w.EndMs != g.EndMs {
			t.Errorf("chapter %d: %+v, want %+v", i+1, g, w)
		}
	}
	if w, g := chapterTrackCount(t, src), chapterTrackCount(t, joined); w != g {
		t.Errorf("the chapter track holds %d samples, the source's %d", g, w)
	}
	if d := sm.DurationMs - jm.DurationMs; d < -1 || d > 1 {
		t.Errorf("duration %d ms after the join, %d before", jm.DurationMs, sm.DurationMs)
	}
	if tags := splitLinkTags(t, joined); len(tags) != 0 {
		t.Errorf("the join carries %v", tags)
	}
	sd, _ := os.ReadFile(src)
	jd, _ := os.ReadFile(joined)
	if ws, gs := trakGroups(t, sd), trakGroups(t, jd); fmt.Sprint(ws) != fmt.Sprint(gs) {
		t.Errorf("alternate groups %v, the source's %v", gs, ws)
	}
	return parts
}

// copyFixture copies a real fixture into the test's directory, where edits may write.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(realFixture(name))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// dressed returns src with what d adds: chapters starting 300 ms ahead of each cut, a cue over the first, and for crowded a second audio and subtitle track.
func dressed(t *testing.T, src string, d dress, cuts []int64) string {
	t.Helper()
	if d == bare {
		return src
	}
	ctx, dir, ext := context.Background(), t.TempDir(), filepath.Ext(src)
	if d == crowded {
		var audio uint64
		for _, tr := range mustMeta(t, src).Tracks {
			if tr.Type == mkv.AudioTrack {
				audio = tr.ID
			}
		}
		if audio != 0 {
			out := filepath.Join(dir, "dual"+ext)
			if err := Merge(ctx, mkv.MergeOptions{OutputPath: out, Inputs: []mkv.MergeInput{{SourcePath: src}, {SourcePath: src, TrackIDs: []uint64{audio}}}}); err != nil {
				t.Fatal(err)
			}
			src = out
		}
	}
	subs := []struct{ lang, from, to string }{{"fre", srtTime(cuts[0] - 300), srtTime(cuts[0] + 500)}}
	if d == crowded {
		subs = append(subs, struct{ lang, from, to string }{"eng", srtTime(100), srtTime(cuts[0] + 200)})
	}
	for i, s := range subs {
		srt := filepath.Join(dir, fmt.Sprintf("%d.srt", i))
		if err := os.WriteFile(srt, []byte("1\n"+s.from+" --> "+s.to+"\nAcross\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, fmt.Sprintf("sub%d%s", i, ext))
		if err := MergeSubtitle(ctx, src, srt, out, s.lang, ""); err != nil {
			t.Fatal(err)
		}
		src = out
	}
	chs := []mkv.Chapter{{ID: 1, Title: "Chapter 1", StartMs: 0}}
	for i, c := range cuts {
		chs = append(chs, mkv.Chapter{ID: uint64(i + 2), Title: fmt.Sprintf("Chapter %d", i+2), StartMs: c - 300})
	}
	if d == crowded {
		end := mustMeta(t, src).DurationMs
		for i := range chs {
			chs[i].EndMs = end
			if i+1 < len(chs) {
				chs[i].EndMs = chs[i+1].StartMs
			}
		}
	}
	if err := EditInPlace(ctx, src, func(c *mkv.Container) { c.Chapters = chs }); err != nil {
		t.Fatal(err)
	}
	return src
}

func srtTime(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}
