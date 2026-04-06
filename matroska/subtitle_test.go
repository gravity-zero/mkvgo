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

func TestExtractSubtitleReal(t *testing.T) {
	const realMKV = "/mnt/e/Cartoon/Serie/Man vs baby/S01/Man vs baby S01E02 - FRENCH - 1080P.mkv"
	if _, err := os.Stat(realMKV); err != nil {
		t.Skipf("real MKV not available: %v", err)
	}

	dir := t.TempDir()
	outPath := dir + "/subs.srt"
	assertNoErr(t, ExtractSubtitle(context.Background(), realMKV, 3, outPath))

	data, err := os.ReadFile(outPath)
	assertNoErr(t, err)

	content := string(data)
	if !strings.Contains(content, "-->") {
		t.Error("SRT file doesn't contain timing markers")
	}
	lines := strings.Split(content, "\n")
	t.Logf("SRT: %d lines, first 10:", len(lines))
	for i := 0; i < 10 && i < len(lines); i++ {
		t.Logf("  %s", lines[i])
	}
}
