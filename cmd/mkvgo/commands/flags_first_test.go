package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestFlagsBeforeTheSource: a command taking its source first reads its flags wherever they stand - before the
// source too - instead of taking the first flag for the source's path.
func TestFlagsBeforeTheSource(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:00,500\nhi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := func(n string) string { return filepath.Join(dir, n) }
	t.Cleanup(func() { Force = false })
	Force = true
	for name, run := range map[string]func(){
		"demux":            func() { CmdDemux([]string{"--raw", "-o", out("demux"), src}) },
		"remove-track":     func() { CmdRemoveTrack([]string{"-o", out("rt.mkv"), "-t", "2", src}) },
		"edit-title":       func() { CmdEditTitle([]string{"-o", out("et.mkv"), src, "T"}) },
		"edit-track":       func() { CmdEditTrack([]string{"-o", out("ek.mkv"), "-t", "1", "-name", "V", src}) },
		"edit":             func() { CmdEdit([]string{"-o", out("e.mkv"), src, `{"title":"x"}`}) },
		"split":            func() { CmdSplit([]string{"-o", out("split"), "-range", "0-0:00.5", src}) },
		"add-track":        func() { CmdAddTrack([]string{"-o", out("at.mkv"), src, src + ":2"}) },
		"merge-subtitle":   func() { CmdMergeSubtitle([]string{"-o", out("ms.mkv"), src, srt}) },
		"extract-subtitle": func() { CmdExtractSubtitle([]string{"-o", out("x.srt"), "-t", "1", "-format", "srt", src}) },
		"subtitle-index":   func() { CmdSubtitleIndex([]string{"-o", out("x.idx"), src}) },
		"extract-attach":   func() { CmdExtractAttachment([]string{"-o", out("a.bin"), src, "1"}) },
	} {
		code, stderr := exitStatusStderr(t, run)
		// The three commands the fixture holds nothing for may refuse it, never for a flag read as a path.
		for _, flag := range []string{"--raw", "\"-o\"", "open -o", "-o: no such file"} {
			if bytes.Contains(stderr, []byte(flag)) {
				t.Errorf("%s: a flag before the source read as a path (exit %d): %s", name, code, stderr)
			}
		}
		if code > 0 && name != "extract-subtitle" && name != "subtitle-index" && name != "extract-attach" {
			t.Errorf("%s with flags before the source: exit %d: %s", name, code, stderr)
		}
	}
}
