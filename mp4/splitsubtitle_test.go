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

// sameSubtitlesAfterJoin joins parts and compares the subtitle samples with src's.
func sameSubtitlesAfterJoin(t *testing.T, src string, parts []string) {
	t.Helper()
	joined := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	ti := textTrackIndex(t, mustCutModel(t, src))
	if want, got := sampleSeqs(t, src)[ti], sampleSeqs(t, joined)[ti]; strings.Join(want, ",") != strings.Join(got, ",") {
		t.Errorf("the join does not give the subtitles back")
		diffFirst(t, want, got)
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
