package commands_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
)

// ---------- FormatBytes -------------------------------------------------------

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1 << 10, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{1<<20 + 1<<19, "1.5 MB"},
		{1 << 30, "1.0 GB"},
		{3 << 30, "3.0 GB"},
	}
	for _, tc := range cases {
		got := commands.FormatBytes(tc.b)
		if got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.b, got, tc.want)
		}
	}
}

// ---------- ParseTrackIDs ----------------------------------------------------

func TestParseTrackIDs(t *testing.T) {
	cases := []struct {
		s    string
		want []uint64
	}{
		{"1", []uint64{1}},
		{"1,2,3", []uint64{1, 2, 3}},
		{" 5 , 10 ", []uint64{5, 10}},
	}
	for _, tc := range cases {
		got := commands.ParseTrackIDs(tc.s)
		if len(got) != len(tc.want) {
			t.Errorf("ParseTrackIDs(%q): len=%d want %d", tc.s, len(got), len(tc.want))
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("ParseTrackIDs(%q)[%d] = %d, want %d", tc.s, i, got[i], tc.want[i])
			}
		}
	}
}

// ---------- ParseTimeRanges --------------------------------------------------

func TestParseTimeRanges(t *testing.T) {
	cases := []struct {
		s    string
		want []matroska.TimeRange
	}{
		{"0-5000", []matroska.TimeRange{{StartMs: 0, EndMs: 5000}}},
		{"100-200", []matroska.TimeRange{{StartMs: 100, EndMs: 200}}},
		{"0-2000,2000-5000", []matroska.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 2000, EndMs: 5000}}},
	}
	for _, tc := range cases {
		got := commands.ParseTimeRanges(tc.s)
		if len(got) != len(tc.want) {
			t.Errorf("ParseTimeRanges(%q): len=%d want %d", tc.s, len(got), len(tc.want))
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("ParseTimeRanges(%q)[%d] = %v, want %v", tc.s, i, got[i], tc.want[i])
			}
		}
	}
}

// ---------- CmdInfo ----------------------------------------------------------

func TestCmdInfo_Text(t *testing.T) {
	path := richMKV(t)
	out := capture(t, func() { commands.CmdInfo(path) })
	for _, want := range []string{
		"Fixture Title",
		"mkvgo-test",
		"Tracks:      3",
		"Chapters:    2",
		"Attachments: 1",
		"Tags:        1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CmdInfo text: missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestCmdInfo_JSON(t *testing.T) {
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdInfo(path) })

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("CmdInfo JSON parse: %v\n%s", err, out)
	}
	if m["title"] != "Fixture Title" {
		t.Errorf("CmdInfo JSON: title = %v, want 'Fixture Title'", m["title"])
	}
	if m["tracks"].(float64) != 3 {
		t.Errorf("CmdInfo JSON: tracks = %v, want 3", m["tracks"])
	}
	if m["chapters"].(float64) != 2 {
		t.Errorf("CmdInfo JSON: chapters = %v, want 2", m["chapters"])
	}
	if m["attachments"].(float64) != 1 {
		t.Errorf("CmdInfo JSON: attachments = %v, want 1", m["attachments"])
	}
}

// ---------- CmdTracks --------------------------------------------------------

func TestCmdTracks_Text(t *testing.T) {
	path := richMKV(t)
	out := capture(t, func() { commands.CmdTracks(path) })
	for _, want := range []string{
		"h264", "aac", "srt",
		"1920x1080",
		"2ch",
		"[default]",
		"Français",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CmdTracks text: missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestCmdTracks_JSON(t *testing.T) {
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdTracks(path) })

	// tracksForJSON wraps tracks; result is an array with embedded Track fields
	// plus codec_long_name and channel_layout.
	var tracks []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &tracks); err != nil {
		t.Fatalf("CmdTracks JSON parse: %v\n%s", err, out)
	}
	if len(tracks) != 3 {
		t.Errorf("CmdTracks JSON: got %d tracks, want 3", len(tracks))
	}
	if tracks[0]["codec"] == nil {
		t.Errorf("CmdTracks JSON: first track missing 'codec' field")
	}
}

// ---------- CmdChapters ------------------------------------------------------

func TestCmdChapters_Text(t *testing.T) {
	path := richMKV(t)
	out := capture(t, func() { commands.CmdChapters(path) })
	for _, want := range []string{"Intro", "Main", "00:00:00", "00:00:02"} {
		if !strings.Contains(out, want) {
			t.Errorf("CmdChapters text: missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestCmdChapters_Empty(t *testing.T) {
	c := &matroska.Container{
		Info:   matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{ID: 1, Type: matroska.VideoTrack, Codec: "h264"}},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdChapters(path) })
	if !strings.Contains(out, "No chapters found.") {
		t.Errorf("CmdChapters empty: expected 'No chapters found.'\noutput:\n%s", out)
	}
}

func TestCmdChapters_JSON(t *testing.T) {
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdChapters(path) })

	var chapters []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &chapters); err != nil {
		t.Fatalf("CmdChapters JSON parse: %v\n%s", err, out)
	}
	if len(chapters) != 2 {
		t.Errorf("CmdChapters JSON: got %d chapters, want 2", len(chapters))
	}
	if chapters[0]["title"] != "Intro" {
		t.Errorf("CmdChapters JSON: chapter[0].title = %v, want 'Intro'", chapters[0]["title"])
	}
}

// ---------- CmdAttachments ---------------------------------------------------

func TestCmdAttachments_Text(t *testing.T) {
	path := richMKV(t)
	out := capture(t, func() { commands.CmdAttachments(path) })
	for _, want := range []string{"cover.jpg", "image/jpeg"} {
		if !strings.Contains(out, want) {
			t.Errorf("CmdAttachments text: missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestCmdAttachments_Empty(t *testing.T) {
	c := &matroska.Container{
		Info:   matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{ID: 1, Type: matroska.VideoTrack, Codec: "h264"}},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdAttachments(path) })
	if !strings.Contains(out, "No attachments found.") {
		t.Errorf("CmdAttachments empty: expected 'No attachments found.'\noutput:\n%s", out)
	}
}

func TestCmdAttachments_JSON(t *testing.T) {
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdAttachments(path) })

	var atts []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &atts); err != nil {
		t.Fatalf("CmdAttachments JSON parse: %v\n%s", err, out)
	}
	if len(atts) != 1 {
		t.Errorf("CmdAttachments JSON: got %d, want 1", len(atts))
	}
	if atts[0]["name"] != "cover.jpg" {
		t.Errorf("CmdAttachments JSON: name = %v, want 'cover.jpg'", atts[0]["name"])
	}
	if atts[0]["mime_type"] != "image/jpeg" {
		t.Errorf("CmdAttachments JSON: mime_type = %v, want 'image/jpeg'", atts[0]["mime_type"])
	}
}

// ---------- CmdTags ----------------------------------------------------------

func TestCmdTags_Text(t *testing.T) {
	path := richMKV(t)
	out := capture(t, func() { commands.CmdTags(path) })
	for _, want := range []string{"MOVIE", "TITLE", "Fixture Title"} {
		if !strings.Contains(out, want) {
			t.Errorf("CmdTags text: missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestCmdTags_Empty(t *testing.T) {
	c := &matroska.Container{
		Info:   matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{ID: 1, Type: matroska.VideoTrack, Codec: "h264"}},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdTags(path) })
	if !strings.Contains(out, "No tags found.") {
		t.Errorf("CmdTags empty: expected 'No tags found.'\noutput:\n%s", out)
	}
}

func TestCmdTags_JSON(t *testing.T) {
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdTags(path) })

	var tags []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &tags); err != nil {
		t.Fatalf("CmdTags JSON parse: %v\n%s", err, out)
	}
	if len(tags) != 1 {
		t.Errorf("CmdTags JSON: got %d tags, want 1", len(tags))
	}
	if tags[0]["target_type"] != "MOVIE" {
		t.Errorf("CmdTags JSON: target_type = %v, want 'MOVIE'", tags[0]["target_type"])
	}
}

// TestCmdTags_TargetIDBranches covers the empty TargetType → "global" path
// and the TargetID > 0 formatting branch, which richMKV does not exercise.
func TestCmdTags_TargetIDBranches(t *testing.T) {
	c := &matroska.Container{
		Info:   matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{ID: 1, Type: matroska.VideoTrack, Codec: "h264"}},
		Tags: []matroska.Tag{
			// empty TargetType → substituted with "global"; TargetID=5 → prints "track=5"
			{TargetType: "", TargetID: 5, SimpleTags: []matroska.SimpleTag{{Name: "COMMENT", Value: "hello"}}},
		},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdTags(path) })
	if !strings.Contains(out, "global") {
		t.Errorf("CmdTags branches: missing 'global'\noutput:\n%s", out)
	}
	if !strings.Contains(out, "track=5") {
		t.Errorf("CmdTags branches: missing 'track=5'\noutput:\n%s", out)
	}
}

// ---------- CmdKeyframes -----------------------------------------------------

// TestCmdKeyframes_TextWithKeyframes covers the per-keyframe printf loop.
// regfixMKV has a Cues index, so keyframes are non-empty; the first is at t=0.
func TestCmdKeyframes_TextWithKeyframes(t *testing.T) {
	out := capture(t, func() { commands.CmdKeyframes(regfixMKV) })
	// First keyframe is at 0 ms → FmtMs(0) = "00:00:00"
	if !strings.Contains(out, "00:00:00") {
		t.Errorf("CmdKeyframes text: expected '00:00:00'\noutput:\n%s", out)
	}
}

// TestCmdKeyframes_TextEmpty covers the "no keyframe index" branch.
// richMKV has no Cues (metadata-only, no frames).
func TestCmdKeyframes_TextEmpty(t *testing.T) {
	path := richMKV(t)
	out := capture(t, func() { commands.CmdKeyframes(path) })
	if !strings.Contains(out, "No keyframe index available") {
		t.Errorf("CmdKeyframes empty: expected 'No keyframe index available'\noutput:\n%s", out)
	}
}

// ---------- CmdProbe JSON (covers containerForJSON / tracksForJSON) -----------

// TestCmdProbeJSON exercises the JSON branch of CmdProbe with an MKV file
// (no dropped tracks), which routes through containerForJSON.
func TestCmdProbeJSON(t *testing.T) {
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdProbe(path) })

	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("CmdProbeJSON parse: %v\n%s", err, out)
	}
	// containerForJSON overrides the embedded tracks field with tracksForJSON output
	tracks, ok := m["tracks"].([]interface{})
	if !ok || len(tracks) != 3 {
		t.Errorf("CmdProbeJSON: expected 3 tracks, got %v", m["tracks"])
	}
}

// ---------- CmdValidate ------------------------------------------------------

func TestCmdValidate_OK(t *testing.T) {
	// regfixMKV is a well-formed ffmpeg-muxed file; Validate should return no issues.
	out := capture(t, func() { commands.CmdValidate(regfixMKV) })
	if !strings.Contains(out, ": OK") {
		t.Errorf("CmdValidate OK: expected ': OK' in output\noutput:\n%s", out)
	}
}

// recordExit installs an exit hook that records the code instead of
// terminating the test binary (validate/compare exit 1 on findings).
func recordExit(t *testing.T) *int {
	t.Helper()
	code := -1
	restore := commands.SetExit(func(c int) { code = c })
	t.Cleanup(restore)
	return &code
}

func TestCmdValidate_WithIssues(t *testing.T) {
	// richMKV is metadata-only (no blocks, video track lacks CodecPrivate)
	// → triggers at least "video without CodecPrivate" and "no blocks found".
	path := richMKV(t)
	exitCode := recordExit(t)
	out := capture(t, func() { commands.CmdValidate(path) })
	if strings.Contains(out, ": OK") {
		t.Errorf("CmdValidate issues: should not contain ': OK'\noutput:\n%s", out)
	}
	if !strings.Contains(out, "[warning]") {
		t.Errorf("CmdValidate issues: expected '[warning]'\noutput:\n%s", out)
	}
	if *exitCode != 1 {
		t.Errorf("CmdValidate with issues: exit code = %d, want 1 (scriptability)", *exitCode)
	}
}

func TestCmdValidate_JSON(t *testing.T) {
	path := richMKV(t)
	recordExit(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdValidate(path) })

	var issues []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		t.Fatalf("CmdValidate JSON parse: %v\n%s", err, out)
	}
	if len(issues) == 0 {
		t.Errorf("CmdValidate JSON: expected non-empty issue list for richMKV")
	}
	for i, iss := range issues {
		if iss["severity"] == nil || iss["message"] == nil {
			t.Errorf("CmdValidate JSON issue[%d]: missing severity or message: %v", i, iss)
		}
	}
}

// ---------- CmdCompare -------------------------------------------------------

func TestCmdCompare_Identical(t *testing.T) {
	pathA := richMKV(t)
	pathB := richMKV(t)
	out := capture(t, func() { commands.CmdCompare(pathA, pathB) })
	if !strings.Contains(out, "identical metadata") {
		t.Errorf("CmdCompare identical: expected 'identical metadata'\noutput:\n%s", out)
	}
}

func TestCmdCompare_Diff(t *testing.T) {
	cA := richContainer()
	cB := richContainer()
	cB.Info.Title = "Different Title"
	pathA := writeMKV(t, cA)
	pathB := writeMKV(t, cB)
	exitCode := recordExit(t)
	out := capture(t, func() { commands.CmdCompare(pathA, pathB) })
	if !strings.Contains(out, "info.title") {
		t.Errorf("CmdCompare diff: expected 'info.title' in output\noutput:\n%s", out)
	}
	if strings.Contains(out, "identical metadata") {
		t.Errorf("CmdCompare diff: should not contain 'identical metadata'\noutput:\n%s", out)
	}
	if *exitCode != 1 {
		t.Errorf("CmdCompare with diffs: exit code = %d, want 1 (scriptability)", *exitCode)
	}
}

func TestCmdCompare_JSON(t *testing.T) {
	cA := richContainer()
	cB := richContainer()
	cB.Info.Title = "Different Title"
	pathA := writeMKV(t, cA)
	pathB := writeMKV(t, cB)
	recordExit(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdCompare(pathA, pathB) })

	var diffs []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &diffs); err != nil {
		t.Fatalf("CmdCompare JSON parse: %v\n%s", err, out)
	}
	if len(diffs) == 0 {
		t.Errorf("CmdCompare JSON: expected at least one diff")
	}
	// every diff must have type, section, detail
	for i, d := range diffs {
		if d["type"] == nil || d["section"] == nil || d["detail"] == nil {
			t.Errorf("CmdCompare JSON diff[%d]: missing field: %v", i, d)
		}
	}
}
