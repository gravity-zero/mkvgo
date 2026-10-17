package commands

import (
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

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

// --damage-bytes sets options().DamageBytes; absent, it is 0 (the library default).
func TestParseHLSFlagsDamageBytes(t *testing.T) {
	if got := parseHLSFlags([]string{"in.mkv"}).options("in.mkv").DamageBytes; got != 0 {
		t.Errorf("options().DamageBytes = %d, want 0 by default", got)
	}
	if got := parseHLSFlags([]string{"in.mkv", "--damage-bytes", "1048576"}).options("in.mkv").DamageBytes; got != 1<<20 {
		t.Errorf("options().DamageBytes = %d, want 1048576", got)
	}
	if got := parseHLSFlags([]string{"in.mkv", "--damage-bytes", "-1"}).options("in.mkv").DamageBytes; got != -1 {
		t.Errorf("options().DamageBytes = %d, want -1 (refuse any damage, as the library)", got)
	}
}

// --source-access sets options().SourceAccess; absent, it is automatic.
func TestParseHLSFlagsSourceAccess(t *testing.T) {
	for flag, want := range map[string]mp4.SourceAccess{"": mp4.SourceAuto, "auto": mp4.SourceAuto, "local": mp4.SourceLocal, "network": mp4.SourceNetwork} {
		args := []string{"in.mkv"}
		if flag != "" {
			args = append(args, "--source-access", flag)
		}
		if got := parseHLSFlags(args).options("in.mkv").SourceAccess; got != want {
			t.Errorf("--source-access %q: %d, want %d", flag, got, want)
		}
	}
}
