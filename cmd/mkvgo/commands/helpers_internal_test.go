package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/matroska"
)

// helpers_internal_test.go - white-box tests for unexported parsing helpers.

// TestSplitTrackSpec locks in that the file:trackID parser splits on the LAST
// colon, so a path that itself contains a colon (a Windows drive letter, or a
// colon-bearing filename on Unix) is preserved. Both CmdMux and CmdAddTrack
// route through this helper, so this guards the whole class of bug at once.
func TestSplitTrackSpec(t *testing.T) {
	cases := []struct {
		in       string
		path, id string
		ok       bool
	}{
		{`sample.mkv:1`, `sample.mkv`, `1`, true},
		{`C:\Users\me\sample.mkv:2`, `C:\Users\me\sample.mkv`, `2`, true},
		{`/tmp/a:b/sample.mkv:3`, `/tmp/a:b/sample.mkv`, `3`, true},
		{`no-colon.mkv`, ``, ``, false},
		{`:5`, ``, ``, false},          // empty path
		{`sample.mkv:`, ``, ``, false}, // empty trackID
		{``, ``, ``, false},
	}
	for _, c := range cases {
		path, id, ok := splitTrackSpec(c.in)
		if ok != c.ok || path != c.path || id != c.id {
			t.Errorf("splitTrackSpec(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, path, id, ok, c.path, c.id, c.ok)
		}
	}
}

// TestLocalProbeLeavesAttachmentsOnDisk: the local probe lists the attachments
// without reading their payloads (a cover can weigh several MB).
func TestLocalProbeLeavesAttachmentsOnDisk(t *testing.T) {
	cover := bytes.Repeat([]byte{0xAB}, 1<<20)
	var buf bytes.Buffer
	if err := matroska.Write(&buf, &matroska.Container{Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks:      []matroska.Track{{ID: 1, Type: matroska.AudioTrack, Codec: "aac"}},
		Attachments: []matroska.Attachment{{ID: 1, Name: "cover.png", MIMEType: "image/png", Data: cover, Size: int64(len(cover))}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cover.mkv")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := loadContainer(path, true)
	if len(c.Attachments) != 1 || c.Attachments[0].Size != 1<<20 || c.Attachments[0].Data != nil || c.Attachments[0].DataOffset == 0 {
		t.Errorf("attachment %+v: want it listed, its 1 MiB payload left on disk", c.Attachments)
	}
}
