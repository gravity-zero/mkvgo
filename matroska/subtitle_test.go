package matroska

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

func TestFormatSRTTime(t *testing.T) {
	for _, tt := range []struct {
		ms   int64
		want string
	}{
		{0, "00:00:00,000"},
		{1500, "00:00:01,500"},
		{3661999, "01:01:01,999"},
	} {
		got := subtitle.FormatSRTTime(tt.ms)
		assertEqual(t, got, tt.want, "formatSRTTime")
	}
}

func TestExtractSubtitleNotFound(t *testing.T) {
	requireFixture(t)
	err := ExtractSubtitle(context.Background(), fixturePath, 99, t.TempDir()+"/nope.srt")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestExtractSubtitleReal is an opt-in smoke test against a real file the
// developer points at — e.g. a real 4K HEVC HDR rip with a text subtitle track.
// No file is committed; set MKVGO_TEST_MKV to run it. It extracts the first
// subtitle track and checks the SRT has timing markers.
func TestExtractSubtitleReal(t *testing.T) {
	path := os.Getenv("MKVGO_TEST_MKV")
	if path == "" {
		t.Skip("set MKVGO_TEST_MKV to a real MKV with a text subtitle track to run")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("MKVGO_TEST_MKV not available: %v", err)
	}

	c, err := OpenMeta(context.Background(), path)
	assertNoErr(t, err)
	var subID uint64
	found := false
	for _, tr := range c.Tracks {
		if tr.Type == SubtitleTrack {
			subID, found = tr.ID, true
			break
		}
	}
	if !found {
		t.Skip("MKVGO_TEST_MKV has no subtitle track")
	}

	outPath := t.TempDir() + "/subs.srt"
	assertNoErr(t, ExtractSubtitle(context.Background(), path, subID, outPath))

	data, err := os.ReadFile(outPath)
	assertNoErr(t, err)
	if !strings.Contains(string(data), "-->") {
		t.Error("SRT file doesn't contain timing markers")
	}
}
