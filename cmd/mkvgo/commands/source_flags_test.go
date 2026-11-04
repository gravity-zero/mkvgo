package commands

import (
	"bytes"
	"path/filepath"
	"testing"
)

// TestSourceOnlyCommandsRejectUnknownFlags: the commands that take a source alone (info, probe, tracks, chapters,
// attachments, keyframes, tags) refuse an unknown flag as unknown, exit 2, before or after the file - it named a file
// before it and went unread after it - and take the global flags (--json, --exit-damaged) anywhere.
func TestSourceOnlyCommandsRejectUnknownFlags(t *testing.T) {
	src, err := filepath.Abs(filepath.Join("..", "..", "..", "internal", "testdata", "sample.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { JsonOutput, ExitDamaged, sawDamage = false, false, false })
	run := map[string]func(string){
		"info": CmdInfo, "probe": CmdProbe, "tracks": CmdTracks, "chapters": CmdChapters,
		"attachments": CmdAttachments, "keyframes": CmdKeyframes, "tags": CmdTags,
	}
	// argv is what main hands over after the command name: the global flags taken, then the source.
	call := func(cmd string, argv ...string) (int, []byte) {
		JsonOutput, ExitDamaged, sawDamage = false, false, false
		return exitStatusStderr(t, func() {
			rest, _ := TakeGlobalFlags(argv)
			run[cmd](SourceArg(cmd, rest))
		})
	}
	for cmd := range run {
		for _, argv := range [][]string{{"--zzbogus", src}, {src, "--zzbogus"}} {
			code, stderr := call(cmd, argv...)
			if code != 2 || !bytes.Contains(stderr, []byte("unknown flag")) {
				t.Errorf("%s %v: exit %d, %s; want 2 and an unknown flag", cmd, argv, code, stderr)
			}
		}
		for _, argv := range [][]string{{"--json", src}, {src, "--json"}, {"--exit-damaged", src, "--json"}} {
			if code, stderr := call(cmd, argv...); code > 0 {
				t.Errorf("%s %v: exit %d, %s; want it read", cmd, argv, code, stderr)
			}
		}
	}
}
