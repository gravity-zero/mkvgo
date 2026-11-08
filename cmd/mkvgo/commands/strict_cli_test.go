package commands_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

const ulawFixture = "../../../internal/testdata/aac_ulaw.mov" // track 1 mu-law (no Matroska form), track 2 AAC

// TestTracksJSONSaysWhatMatroskaCannotCarry: tracks -json and probe -json say, per track of an MP4, whether the
// remux to Matroska carries it and, when not, why.
func TestTracksJSONSaysWhatMatroskaCannotCarry(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	check := func(name string, tracks []map[string]any) {
		t.Helper()
		if len(tracks) != 2 {
			t.Fatalf("%s: %d tracks, want 2", name, len(tracks))
		}
		if v, _ := tracks[0]["to_matroska"].(bool); v || !strings.Contains(tracks[0]["to_matroska_refusal"].(string), "ulaw") {
			t.Errorf("%s: track 1 = %v, want to_matroska false with the mu-law reason", name, tracks[0])
		}
		if v, _ := tracks[1]["to_matroska"].(bool); !v || tracks[1]["to_matroska_refusal"] != nil {
			t.Errorf("%s: track 2 = %v, want to_matroska true and no refusal", name, tracks[1])
		}
	}
	var tracks []map[string]any
	if err := json.Unmarshal(stdoutOf(t, func() { commands.CmdTracks(ulawFixture) }), &tracks); err != nil {
		t.Fatal(err)
	}
	check("tracks", tracks)
	var probe struct {
		Tracks []map[string]any `json:"tracks"`
	}
	if err := json.Unmarshal(stdoutOf(t, func() { commands.CmdProbe(ulawFixture) }), &probe); err != nil {
		t.Fatal(err)
	}
	check("probe", probe.Tracks)
}

// stdoutOf returns what fn wrote to stdout.
func stdoutOf(t *testing.T, fn func()) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	fn()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStrictRefusesDroppedTracks: --strict fails from-mp4 and demux on a track they would drop, naming it and the
// flag; the default drops it with a dropped track line.
func TestStrictRefusesDroppedTracks(t *testing.T) {
	dir := t.TempDir()
	msg := fatalMessage(t, 1, func() {
		commands.CmdFromMP4([]string{"--strict", "--skip-unsupported", ulawFixture, filepath.Join(dir, "out.mkv")})
	})
	if !strings.Contains(msg, "track 1 (ulaw)") || !strings.Contains(msg, "--strict") || strings.Contains(msg, "Options.") {
		t.Errorf("from-mp4 --strict: %q, want track 1 (ulaw) refused by --strict", msg)
	}
	msg = fatalMessage(t, 1, func() {
		commands.CmdDemux([]string{"--strict", "--skip-unsupported", ulawFixture, "-o", filepath.Join(dir, "demux")})
	})
	if !strings.Contains(msg, "track 1 (ulaw)") || !strings.Contains(msg, "--strict") || strings.Contains(msg, "Options.") {
		t.Errorf("demux --strict: %q, want track 1 (ulaw) refused by --strict", msg)
	}
}

const twoVideoASSFixture = "../../../internal/testdata/two_video_ass.mkv" // two H.264 video tracks, an ASS subtitle

// TestTracksJSONSaysWhatMP4AndHLSCarry: on a Matroska, tracks -json says per track whether to-mp4 and to-hls/serve
// carry it and why not, the reasons naming the flags; an MP4 source has no such fields.
func TestTracksJSONSaysWhatMP4AndHLSCarry(t *testing.T) {
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	var tracks []map[string]any
	if err := json.Unmarshal(stdoutOf(t, func() { commands.CmdTracks(twoVideoASSFixture) }), &tracks); err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("%d tracks, want 3", len(tracks))
	}
	if tracks[0]["to_mp4"] != true || tracks[0]["to_hls"] != true {
		t.Errorf("track 1 = %v, want to_mp4 and to_hls true", tracks[0])
	}
	if tracks[1]["to_mp4"] != true || tracks[1]["to_hls"] != false || !strings.Contains(tracks[1]["to_hls_refusal"].(string), "secondary video") {
		t.Errorf("track 2 = %v, want to_hls false as a secondary video", tracks[1])
	}
	reason, _ := tracks[2]["to_mp4_refusal"].(string)
	if tracks[2]["to_mp4"] != false || !strings.Contains(reason, "--flatten-subs") || strings.Contains(reason, "Options.") || tracks[2]["to_hls"] != true {
		t.Errorf("track 3 = %v, want to_mp4 false naming --flatten-subs and to_hls true", tracks[2])
	}
	var mp4Tracks []map[string]any
	if err := json.Unmarshal(stdoutOf(t, func() { commands.CmdTracks(ulawFixture) }), &mp4Tracks); err != nil {
		t.Fatal(err)
	}
	if _, ok := mp4Tracks[0]["to_mp4"]; ok {
		t.Errorf("an MP4 source carries to_mp4: %v", mp4Tracks[0])
	}
}

// TestStrictRefusesDropsTowardsMP4AndHLS: --strict fails to-mp4 and to-hls on a track they would drop, naming it
// and the flag.
func TestStrictRefusesDropsTowardsMP4AndHLS(t *testing.T) {
	dir := t.TempDir()
	msg := fatalMessage(t, 1, func() { commands.CmdToMP4([]string{"--strict", twoVideoASSFixture, filepath.Join(dir, "out.mp4")}) })
	if !strings.Contains(msg, "track 3 (ass)") || !strings.Contains(msg, "--strict") || strings.Contains(msg, "Options.") {
		t.Errorf("to-mp4 --strict: %q, want track 3 (ass) refused by --strict", msg)
	}
	msg = fatalMessage(t, 1, func() { commands.CmdToHLS([]string{"--strict", twoVideoASSFixture, "-o", filepath.Join(dir, "hls")}) })
	if !strings.Contains(msg, "track 2 (h264)") || !strings.Contains(msg, "--strict") || strings.Contains(msg, "Options.") {
		t.Errorf("to-hls --strict: %q, want track 2 (h264) refused by --strict", msg)
	}
}
