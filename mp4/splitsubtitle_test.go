package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// textTrackIndex is the position of a cut model's timed-text track.
func textTrackIndex(t *testing.T, m *cutModel) int {
	t.Helper()
	for i := range m.tracks {
		if h := m.tracks[i].handler; h == "text" || h == "sbtl" {
			return i
		}
	}
	t.Fatal("no timed-text track")
	return -1
}

// subtitled is the edit fixture with one SRT cue merged in.
func subtitled(t *testing.T, cue string) string {
	t.Helper()
	srt := filepath.Join(t.TempDir(), "s.srt")
	if err := os.WriteFile(srt, []byte("1\n"+cue+"\nAcross\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "sub.mp4")
	if err := MergeSubtitle(context.Background(), editFixture(t), srt, src, "fre", ""); err != nil {
		t.Fatal(err)
	}
	return src
}

// sameSubtitlesAfterJoin joins parts and compares every track's samples with src's: a cue running past a cut must not move the picture's seam.
func sameSubtitlesAfterJoin(t *testing.T, src string, parts []string) {
	t.Helper()
	joined := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	want, got := sampleSeqs(t, src), sampleSeqs(t, joined)
	for ti := range want {
		if strings.Join(want[ti], ",") != strings.Join(got[ti], ",") {
			t.Errorf("track %d: the join does not give the source back", ti+1)
			diffFirst(t, want[ti], got[ti])
		}
	}
}

// TestSplitCarriesASubtitleAcrossTheCut: a cue running over a cut shows at the head of the next part too, and a join gives the source's subtitle samples back.
func TestSplitCarriesASubtitleAcrossTheCut(t *testing.T) {
	src := subtitled(t, "00:00:02,500 --> 00:00:03,500")
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})

	m := mustCutModel(t, parts[1])
	c := &m.tracks[textTrackIndex(t, m)]
	data, err := os.ReadFile(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(c.tt.samples) == 0 {
		t.Errorf("part 2 has no subtitle sample, want the cue running over the cut")
	} else if first, pt := c.tt.samples[0], c.ptNs(0, m.movieTS); pt > 0 || !bytes.Contains(data[first.src:first.src+int64(first.size)], []byte("Across")) {
		t.Errorf("part 2 opens its subtitles at %d ns on %q, want the cue running over the cut at 0", pt, data[first.src:first.src+int64(first.size)])
	}
	sameSubtitlesAfterJoin(t, src, parts)
}

// TestSplitCarriesASubtitleOverTwoCuts: a cue spanning a whole part opens it and the next, and comes back once.
func TestSplitCarriesASubtitleOverTwoCuts(t *testing.T) {
	src := subtitled(t, "00:00:01,500 --> 00:00:04,500")
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 4000}, {StartMs: 4000}}})
	for _, p := range parts[1:] {
		m := mustCutModel(t, p)
		if c := &m.tracks[textTrackIndex(t, m)]; len(c.tt.samples) == 0 || c.ptNs(0, m.movieTS) > 0 {
			t.Errorf("%s does not open on the cue", filepath.Base(p))
		}
	}
	sameSubtitlesAfterJoin(t, src, parts)
}

// TestJoinOfUnrelatedFilesKeepsEachFilesSubtitleTimes: a cue the previous file holds past where the next file's first cue starts does not push the next file's subtitles late.
func TestJoinOfUnrelatedFilesKeepsEachFilesSubtitleTimes(t *testing.T) {
	src := subtitled(t, "00:00:02,500 --> 00:00:03,500")
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	twice := filepath.Join(t.TempDir(), "twice.mp4")
	if err := Join(context.Background(), []string{parts[0], parts[0]}, twice); err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), []string{twice, parts[1]}, joined); err != nil {
		t.Fatal(err)
	}
	offset := mustMeta(t, twice).DurationMs * 1_000_000
	p, j := mustCutModel(t, parts[1]), mustCutModel(t, joined)
	pc, jc := &p.tracks[textTrackIndex(t, p)], &j.tracks[textTrackIndex(t, j)]
	n, m := len(pc.tt.samples), len(jc.tt.samples)
	if m < n {
		t.Fatalf("the join holds %d subtitle samples, the last file alone %d", m, n)
	}
	for i := 0; i < n; i++ {
		want, got := offset+pc.ptNs(i, p.movieTS), jc.ptNs(m-n+i, j.movieTS)
		if d := got - want; d < -1_000_000 || d > 1_000_000 {
			t.Errorf("subtitle sample %d of the last file presents at %d ms in the join, want %d", i+1, got/1_000_000, want/1_000_000)
		}
	}
}
