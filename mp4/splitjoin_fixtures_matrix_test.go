package mp4

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
					checkSplitJoin(t, src, ranges, d != bare)
				})
			}
		}
	}
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
				checkSplitJoin(t, src, []mkv.TimeRange{{StartMs: 0, EndMs: cuts[0]}, {StartMs: cuts[0], EndMs: cuts[1]}, {StartMs: cuts[1]}}, d != bare)
			})
		}
	}
}

// checkSplitJoin splits src at ranges and requires the join to give back its samples, chapters, chapter track, groups and length, with no link left; cued: the second part opens on a cue.
func checkSplitJoin(t *testing.T, src string, ranges []mkv.TimeRange, cued bool) {
	t.Helper()
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: ranges})
	if len(parts) != len(ranges) {
		t.Fatalf("%d parts, want %d", len(parts), len(ranges))
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
