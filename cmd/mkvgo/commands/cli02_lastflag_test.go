package commands_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

func TestCLI02_ValueFlagLast(t *testing.T) {
	src := "../../../internal/testdata/sample.mkv"
	out := filepath.Join(t.TempDir(), "out")
	cases := map[string]func(){
		"to-hls -segment":          func() { commands.CmdToHLS([]string{src, "-o", out, "-segment"}) },
		"to-hls --aes-key":         func() { commands.CmdToHLS([]string{src, "-o", out, "--aes-key"}) },
		"to-hls -o":                func() { commands.CmdToHLS([]string{src, "-o"}) },
		"hls-segment --url-prefix": func() { commands.CmdHLSSegment([]string{src, "-o", out, "master.m3u8", "--url-prefix"}) },
		"extract-frames -o":        func() { commands.CmdExtractFrames([]string{src, "-o"}) },
	}
	for name, run := range cases {
		msg := fatalMessage(t, run)
		flag := name[strings.LastIndex(name, " ")+1:]
		if !strings.Contains(msg, flag) {
			t.Errorf("%s: the error does not name the flag: %q", name, msg)
		}
	}
}
