package commands

import "testing"

// TestParseHLSFlagsChapterMarkers checks --chapter-markers is a boolean
// switch (absent = false, the default) that rides through to
// Options.ChapterMarkers.
func TestParseHLSFlagsChapterMarkers(t *testing.T) {
	if f := parseHLSFlags([]string{"in.mkv"}); f.chapterMarkers {
		t.Error("chapterMarkers must default to false")
	}
	if got := parseHLSFlags([]string{"in.mkv"}).options("in.mkv").ChapterMarkers; got {
		t.Error("options().ChapterMarkers must default to false")
	}
	f := parseHLSFlags([]string{"in.mkv", "--chapter-markers"})
	if !f.chapterMarkers {
		t.Error("--chapter-markers must set chapterMarkers = true")
	}
	if got := f.options("in.mkv").ChapterMarkers; !got {
		t.Error("options().ChapterMarkers must be true")
	}
}

// TestParseHLSFlagsNoIFrames checks --no-iframes is a boolean switch (absent =
// false, the default) that rides through to Options.NoIFrames.
func TestParseHLSFlagsNoIFrames(t *testing.T) {
	if got := parseHLSFlags([]string{"in.mkv"}).options("in.mkv").NoIFrames; got {
		t.Error("options().NoIFrames must default to false")
	}
	if got := parseHLSFlags([]string{"in.mkv", "--no-iframes"}).options("in.mkv").NoIFrames; !got {
		t.Error("--no-iframes must set options().NoIFrames")
	}
}
