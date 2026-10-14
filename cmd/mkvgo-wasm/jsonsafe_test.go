package main

import (
	"encoding/json"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestJSSafeJSONKeepsUIDs: the UIDs reach JavaScript as exact strings, track
// numbers and ordinary integers stay numbers, any integer past 2^53 - 1 is a string.
func TestJSSafeJSONKeepsUIDs(t *testing.T) {
	const uid = 7454167688999517126
	c := mkv.Container{
		Tracks:      []mkv.Track{{ID: 1, UID: uid, Type: mkv.VideoTrack}},
		Chapters:    []mkv.Chapter{{ID: uid, SubChapters: []mkv.Chapter{{ID: 3}}}},
		Attachments: []mkv.Attachment{{ID: uid, Size: 25}},
		Tags: []mkv.Tag{{TargetType: "TRACK", TargetID: uid, SimpleTags: []mkv.SimpleTag{
			{Name: `quote " and \\ "uid":1`, Value: "-12.5e3"}}}},
		DurationMs: 1 << 60,
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	out := jsSafeJSON(raw)
	var got struct {
		Tracks      []map[string]any `json:"tracks"`
		Chapters    []map[string]any `json:"chapters"`
		Attachments []map[string]any `json:"attachments"`
		Tags        []map[string]any `json:"tags"`
		DurationMs  any              `json:"duration_ms"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	const want = "7454167688999517126"
	for name, v := range map[string]any{
		"track uid":     got.Tracks[0]["uid"],
		"chapter id":    got.Chapters[0]["id"],
		"attachment id": got.Attachments[0]["id"],
		"tag target_id": got.Tags[0]["target_id"],
	} {
		if v != want {
			t.Errorf("%s = %#v, want the string %q", name, v, want)
		}
	}
	if sub := got.Chapters[0]["sub_chapters"].([]any)[0].(map[string]any)["id"]; sub != "3" {
		t.Errorf("sub-chapter id = %#v, want the string \"3\" (a UID even when small)", sub)
	}
	if id := got.Tracks[0]["id"]; id != float64(1) {
		t.Errorf("track id (TrackNumber) = %#v, want the number 1", id)
	}
	if size := got.Attachments[0]["size"]; size != float64(25) {
		t.Errorf("attachment size = %#v, want the number 25", size)
	}
	if st := got.Tags[0]["simple_tags"].([]any)[0].(map[string]any); st["name"] != `quote " and \\ "uid":1` || st["value"] != "-12.5e3" {
		t.Errorf("strings must pass through untouched: %#v", st)
	}
	if d := got.DurationMs; d != "1152921504606846976" {
		t.Errorf("duration_ms past 2^53 = %#v, want a string", d)
	}
}
