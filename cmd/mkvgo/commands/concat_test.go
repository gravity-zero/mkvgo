package commands_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// concat-hls needs at least two sources and an output directory.
func TestCLIConcatHLS_UsageErrors(t *testing.T) {
	mustFatal(t, func() { commands.CmdConcatHLS([]string{"-o", t.TempDir(), regfixMKV}) }) // one source
	mustFatal(t, func() { commands.CmdConcatHLS([]string{regfixMKV, regfixMKV}) })         // no -o
}

// concat-segment needs a resource name and at least two sources.
func TestCLIConcatSegment_UsageErrors(t *testing.T) {
	mustFatal(t, func() { commands.CmdConcatSegment([]string{"master.m3u8", regfixMKV}) }) // one source
	mustFatal(t, func() { commands.CmdConcatSegment([]string{regfixMKV, regfixMKV}) })     // no resource name (< 3 args)
}

// A happy path through both commands: concat-hls packages two copies of the
// same fixture into one continuous session, and concat-segment serves the
// same master on demand.
func TestCLIConcatHLSAndSegment(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "stream")
	commands.CmdConcatHLS([]string{regfixMKV, regfixMKV, "-o", out, "-segment", "2"})

	master, err := os.ReadFile(filepath.Join(out, "master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(master), "#EXT-X-STREAM-INF:") {
		t.Errorf("concat-hls master looks wrong:\n%s", master)
	}
	pl, err := os.ReadFile(filepath.Join(out, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pl), "#EXT-X-DISCONTINUITY") {
		t.Errorf("concat-hls playlist missing DISCONTINUITY:\n%s", pl)
	}

	segOut := filepath.Join(dir, "master-on-demand.m3u8")
	commands.CmdConcatSegment([]string{"master.m3u8", regfixMKV, regfixMKV, "-o", segOut, "-segment", "2"})
	onDemand, err := os.ReadFile(segOut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDemand), "#EXT-X-STREAM-INF:") {
		t.Errorf("concat-segment master looks wrong:\n%s", onDemand)
	}

	p0InitOut := filepath.Join(dir, "p0-init.mp4")
	commands.CmdConcatSegment([]string{"p0/init.mp4", regfixMKV, regfixMKV, "-o", p0InitOut, "-segment", "2"})
	got, err := os.ReadFile(p0InitOut)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(out, "p0", "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("concat-segment p0/init.mp4 differs from concat-hls's own p0/init.mp4")
	}
}
