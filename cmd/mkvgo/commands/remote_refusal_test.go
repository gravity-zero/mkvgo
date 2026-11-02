package commands

import (
	"strings"
	"testing"
)

// TestLocalOnlyCommandsRefuseARemoteSource: a command that reads the whole
// local file refuses an http(s)/s3:// source by saying so and naming the
// commands that take one - not with the bare open error of a path that does
// not exist.
func TestLocalOnlyCommandsRefuseARemoteSource(t *testing.T) {
	c := captureFatals(t)
	const url = "https://example.invalid/movie.mkv"
	for name, run := range map[string]func(){
		"cue-health": func() { CmdCueHealth([]string{url}) },
		"track-ends": func() { CmdTrackEnds([]string{url}) },
		"diagnose":   func() { CmdDiagnose([]string{url}) },
		"validate":   func() { CmdValidate([]string{url}) },
		"verify":     func() { CmdVerify([]string{url}) },
		"hash":       func() { CmdHash([]string{url}) },
		"compare":    func() { CmdCompare([]string{"-blocks", url, "local.mkv"}) },
		"salvage":    func() { CmdSalvage([]string{url, "--dry-run"}) },
	} {
		msg := c.run(run)
		if !strings.Contains(msg, "needs the file on a local disk") || !strings.Contains(msg, "info") {
			t.Errorf("%s: %q", name, msg)
		}
	}
}
