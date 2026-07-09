package matroska

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFacadeExtractSubtitleWebVTT proves ExtractSubtitleWebVTT delegates and
// produces WebVTT text from an embedded text subtitle track.
func TestFacadeExtractSubtitleWebVTT(t *testing.T) {
	if _, err := os.Stat(subsFixturePath); err != nil {
		t.Skipf("fixture not available: %v", err)
	}
	c, err := Open(context.Background(), subsFixturePath)
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
		t.Skip("fixture has no subtitle track")
	}

	var buf strings.Builder
	assertNoErr(t, ExtractSubtitleWebVTT(context.Background(), subsFixturePath, subID, &buf))
	if !strings.HasPrefix(buf.String(), "WEBVTT") {
		t.Errorf("output does not start with WEBVTT:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "-->") {
		t.Errorf("output has no timing markers:\n%s", buf.String())
	}
}

// TestFacadeSubtitleFileToWebVTT proves SubtitleFileToWebVTT delegates and
// converts an external .srt sidecar to WebVTT.
func TestFacadeSubtitleFileToWebVTT(t *testing.T) {
	srtPath := filepath.Join(t.TempDir(), "sub.srt")
	srtContent := "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
	assertNoErr(t, os.WriteFile(srtPath, []byte(srtContent), 0o644))

	var buf strings.Builder
	assertNoErr(t, SubtitleFileToWebVTT(srtPath, &buf))
	if !strings.HasPrefix(buf.String(), "WEBVTT") {
		t.Errorf("output does not start with WEBVTT:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Hello") {
		t.Errorf("output missing cue text:\n%s", buf.String())
	}
}

// TestFacadeExtractKeyframeSample proves ExtractKeyframeSample delegates and
// returns a decoder-ready keyframe sample for the fixture's video track.
func TestFacadeExtractKeyframeSample(t *testing.T) {
	requireFixture(t)
	sample, err := ExtractKeyframeSample(context.Background(), fixturePath, 0)
	assertNoErr(t, err)
	if sample.Codec != "h264" {
		t.Errorf("Codec = %q, want h264", sample.Codec)
	}
	if len(sample.Data) == 0 {
		t.Error("Data is empty")
	}
	if sample.Ext == "" {
		t.Error("Ext is empty")
	}
}
