package commands_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// partialCuesMKV writes a file whose Cues index the first of the two keyframes in each Cluster.
func partialCuesMKV(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "partial.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mw := writer.NewMKVWriter(f)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(mw.WriteStart())
	must(mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}}, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264"}}, 2000))
	for _, tc := range []int64{0, 1000} {
		must(mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte{1}},
			{TrackNumber: 1, Timecode: tc + 500, Keyframe: true, Data: []byte{2}},
		}))
	}
	must(mw.Finalize())
	return path
}

// TestCmdKeyframesSaysTheCuesMayMissSome pins that a list taken from the Cues says so on stderr, and that -walk
// lists the keyframes they leave out.
func TestCmdKeyframesSaysTheCuesMayMissSome(t *testing.T) {
	path := partialCuesMKV(t)
	var out string
	msg := stderrOf(t, func() { out = capture(t, func() { commands.CmdKeyframesArgs([]string{path}) }) })
	if strings.Count(out, "\n") != 2 || !strings.Contains(msg, "listed from the Cues index (2 entries)") || !strings.Contains(msg, "-walk") {
		t.Errorf("from the Cues: stdout %q, stderr %q", out, msg)
	}
	msg = stderrOf(t, func() { out = capture(t, func() { commands.CmdKeyframesArgs([]string{"-walk", path}) }) })
	if !strings.Contains(out, "  500\n") || !strings.Contains(out, "  1500\n") || strings.Count(out, "\n") != 4 || msg != "" {
		t.Errorf("-walk: stdout %q, stderr %q; want the 4 keyframes and no note", out, msg)
	}
}
