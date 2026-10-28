package commands_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestCLIInfoTruncated pins that info names a source cut short in both its text and its
// JSON, for Matroska and MP4, and says nothing of a whole file or of surplus bytes past its end.
func TestCLIInfoTruncated(t *testing.T) {
	dir := t.TempDir()
	mkvSrc := "../../../internal/testdata/sample.mkv"
	mp4Src := filepath.Join(dir, "whole.mp4")
	if err := mp4.RemuxToMP4(context.Background(), mkvSrc, mp4Src, mp4.Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	variant := func(src, name string, edit func([]byte) []byte) string {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, edit(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cut := func(b []byte) []byte { return b[:len(b)*7/10] }
	junk := func(b []byte) []byte { return append(append([]byte{}, b...), make([]byte, 33)...) }
	cases := []struct {
		path string
		cut  bool
	}{
		{mkvSrc, false},
		{variant(mkvSrc, "cut.mkv", cut), true},
		{variant(mkvSrc, "junk.mkv", junk), false},
		{mp4Src, false},
		{variant(mp4Src, "cut.mp4", cut), true},
		{variant(mp4Src, "junk.mp4", junk), false},
	}
	for _, tc := range cases {
		text := capture(t, func() { commands.CmdInfo(tc.path) })
		commands.JsonOutput = true
		js := capture(t, func() { commands.CmdInfo(tc.path) })
		commands.JsonOutput = false
		if got := strings.Contains(text, "Truncated:"); got != tc.cut {
			t.Errorf("%s: text names a cut = %v, want %v:\n%s", filepath.Base(tc.path), got, tc.cut, text)
		}
		if got := strings.Contains(js, `"truncated"`); got != tc.cut {
			t.Errorf("%s: JSON names a cut = %v, want %v:\n%s", filepath.Base(tc.path), got, tc.cut, js)
		}
	}
}
