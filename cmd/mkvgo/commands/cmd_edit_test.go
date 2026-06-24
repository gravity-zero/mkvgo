package commands_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
)

// openEdited is a small helper that opens outPath with matroska.Open and fails
// the test immediately if the file cannot be parsed.
func openEdited(t *testing.T, outPath string) *matroska.Container {
	t.Helper()
	c, err := matroska.Open(context.Background(), outPath)
	if err != nil {
		t.Fatalf("open edited output %q: %v", outPath, err)
	}
	return c
}

// trackByID returns the first Track in c whose ID equals id, or fails the test.
func trackByID(t *testing.T, c *matroska.Container, id uint64) matroska.Track {
	t.Helper()
	for _, tr := range c.Tracks {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("track %d not found in container", id)
	return matroska.Track{}
}

// -------------------------------------------------------------------
// applyPatch — exercised via CmdEdit (table-driven JSON round-trips)
// -------------------------------------------------------------------

// TestApplyPatch_ViaEdit exercises every code path of applyPatch through CmdEdit:
// each case writes a richMKV fixture, applies a JSON patch, re-opens the output
// and asserts the change took effect.
func TestApplyPatch_ViaEdit(t *testing.T) {
	tests := []struct {
		name   string
		patch  string
		verify func(t *testing.T, c *matroska.Container)
	}{
		{
			name:  "title",
			patch: `{"title":"Patched Title"}`,
			verify: func(t *testing.T, c *matroska.Container) {
				if c.Info.Title != "Patched Title" {
					t.Errorf("title = %q, want %q", c.Info.Title, "Patched Title")
				}
			},
		},
		{
			name:  "track_language",
			patch: `{"tracks":[{"id":2,"language":"fra"}]}`,
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 2)
				if tr.Language != "fra" {
					t.Errorf("track 2 language = %q, want fra", tr.Language)
				}
			},
		},
		{
			name:  "track_name",
			patch: `{"tracks":[{"id":3,"name":"English Sub"}]}`,
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 3)
				if tr.Name != "English Sub" {
					t.Errorf("track 3 name = %q, want %q", tr.Name, "English Sub")
				}
			},
		},
		{
			name:  "track_flags_default_forced",
			patch: `{"tracks":[{"id":2,"is_default":false,"is_forced":true}]}`,
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 2)
				if tr.IsDefault {
					t.Error("track 2 IsDefault = true, want false")
				}
				if !tr.IsForced {
					t.Error("track 2 IsForced = false, want true")
				}
			},
		},
		{
			name:  "chapters_replace",
			patch: `{"chapters":[{"id":1,"title":"Only","start_ms":0,"end_ms":1000}]}`,
			verify: func(t *testing.T, c *matroska.Container) {
				if len(c.Chapters) != 1 {
					t.Fatalf("chapters = %d, want 1", len(c.Chapters))
				}
				if c.Chapters[0].Title != "Only" {
					t.Errorf("chapter title = %q, want %q", c.Chapters[0].Title, "Only")
				}
				if c.Chapters[0].EndMs != 1000 {
					t.Errorf("chapter end_ms = %d, want 1000", c.Chapters[0].EndMs)
				}
			},
		},
		{
			name:  "tags_replace",
			patch: `{"tags":[{"target_type":"EPISODE","simple_tags":[{"name":"TITLE","value":"Ep1"}]}]}`,
			verify: func(t *testing.T, c *matroska.Container) {
				if len(c.Tags) != 1 {
					t.Fatalf("tags = %d, want 1", len(c.Tags))
				}
				if c.Tags[0].TargetType != "EPISODE" {
					t.Errorf("tag target_type = %q, want EPISODE", c.Tags[0].TargetType)
				}
				if len(c.Tags[0].SimpleTags) != 1 || c.Tags[0].SimpleTags[0].Value != "Ep1" {
					t.Errorf("tag simple_tags unexpected: %+v", c.Tags[0].SimpleTags)
				}
			},
		},
		{
			// {} patch: all pointer fields are nil → applyPatch must be a no-op.
			name:  "empty_patch_no_change",
			patch: `{}`,
			verify: func(t *testing.T, c *matroska.Container) {
				if c.Info.Title != "Fixture Title" {
					t.Errorf("title changed to %q, expected no change", c.Info.Title)
				}
				// richContainer has 2 chapters; they must be untouched.
				if len(c.Chapters) != 2 {
					t.Errorf("chapters = %d, want 2 (unchanged)", len(c.Chapters))
				}
				// richContainer has 1 tag; it must be untouched.
				if len(c.Tags) != 1 {
					t.Errorf("tags = %d, want 1 (unchanged)", len(c.Tags))
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			src := richMKV(t)
			out := filepath.Join(t.TempDir(), "out.mkv")
			stdout := capture(t, func() {
				commands.CmdEdit([]string{src, "-o", out, tc.patch})
			})
			if !strings.Contains(stdout, "edited") {
				t.Errorf("CmdEdit stdout missing 'edited': %q", stdout)
			}
			c := openEdited(t, out)
			tc.verify(t, c)
		})
	}
}

// -------------------------------------------------------------------
// CmdEditTitle
// -------------------------------------------------------------------

func TestCmdEditTitle(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "titled.mkv")

	stdout := capture(t, func() {
		commands.CmdEditTitle([]string{src, "-o", out, "My Movie"})
	})
	if !strings.Contains(stdout, "My Movie") {
		t.Errorf("CmdEditTitle stdout = %q; want title string in output", stdout)
	}

	c := openEdited(t, out)
	if c.Info.Title != "My Movie" {
		t.Errorf("title = %q, want %q", c.Info.Title, "My Movie")
	}
	// Tracks must be preserved.
	if len(c.Tracks) != 3 {
		t.Errorf("tracks = %d after title edit, want 3", len(c.Tracks))
	}
}

// -------------------------------------------------------------------
// CmdEditTrack
// -------------------------------------------------------------------

func TestCmdEditTrack(t *testing.T) {
	tests := []struct {
		name    string
		extra   []string // appended after [src, "-o", out]
		verify  func(t *testing.T, c *matroska.Container)
	}{
		{
			name:  "language",
			extra: []string{"-t", "2", "-lang", "fra"},
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 2)
				if tr.Language != "fra" {
					t.Errorf("track 2 language = %q, want fra", tr.Language)
				}
			},
		},
		{
			name:  "name",
			extra: []string{"-t", "3", "-name", "Commentary"},
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 3)
				if tr.Name != "Commentary" {
					t.Errorf("track 3 name = %q, want Commentary", tr.Name)
				}
			},
		},
		{
			name:  "no_default",
			extra: []string{"-t", "1", "-no-default"},
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 1)
				if tr.IsDefault {
					t.Error("track 1 IsDefault = true, want false after -no-default")
				}
			},
		},
		{
			name:  "forced",
			extra: []string{"-t", "3", "-forced"},
			verify: func(t *testing.T, c *matroska.Container) {
				tr := trackByID(t, c, 3)
				if !tr.IsForced {
					t.Error("track 3 IsForced = false, want true after -forced")
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			src := richMKV(t)
			out := filepath.Join(t.TempDir(), "out.mkv")
			args := append([]string{src, "-o", out}, tc.extra...)
			stdout := capture(t, func() { commands.CmdEditTrack(args) })
			if !strings.Contains(stdout, "edited track") {
				t.Errorf("CmdEditTrack stdout = %q; want 'edited track'", stdout)
			}
			c := openEdited(t, out)
			tc.verify(t, c)
		})
	}
}

// -------------------------------------------------------------------
// CmdEditInPlace
// -------------------------------------------------------------------

func TestCmdEditInPlace(t *testing.T) {
	// Use a shorter title so the new metadata fits in the existing region
	// (richMKV has no Void padding, so the patch must shrink or equal the old size).
	src := richMKV(t)

	stdout := capture(t, func() {
		commands.CmdEditInPlace([]string{src, `{"title":"X"}`})
	})
	if !strings.Contains(stdout, "edited in-place") {
		t.Errorf("CmdEditInPlace stdout = %q; want 'edited in-place'", stdout)
	}

	c := openEdited(t, src)
	if c.Info.Title != "X" {
		t.Errorf("title after in-place edit = %q, want X", c.Info.Title)
	}
	// Tracks must still be intact.
	if len(c.Tracks) != 3 {
		t.Errorf("tracks = %d after in-place edit, want 3", len(c.Tracks))
	}
}

// TestCmdEditInPlace_TrackLanguage verifies that a track-level patch also
// persists through in-place editing.
func TestCmdEditInPlace_TrackLanguage(t *testing.T) {
	src := richMKV(t)
	capture(t, func() {
		commands.CmdEditInPlace([]string{src, `{"tracks":[{"id":2,"language":"deu"}]}`})
	})
	c := openEdited(t, src)
	tr := trackByID(t, c, 2)
	if tr.Language != "deu" {
		t.Errorf("track 2 language = %q, want deu", tr.Language)
	}
}

// -------------------------------------------------------------------
// CmdExtractAttachment
// -------------------------------------------------------------------

func TestCmdExtractAttachment(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "cover.jpg")

	stdout := capture(t, func() {
		commands.CmdExtractAttachment([]string{src, "1", "-o", out})
	})
	if !strings.Contains(stdout, "extracted attachment") {
		t.Errorf("CmdExtractAttachment stdout = %q; want 'extracted attachment'", stdout)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	want := []byte{1, 2, 3}
	if !bytes.Equal(data, want) {
		t.Errorf("attachment data = %v, want %v", data, want)
	}
}

// -------------------------------------------------------------------
// CmdExtractSubtitle / extractWebVTT
// -------------------------------------------------------------------

// TestCmdExtractSubtitle_SRT verifies that SRT extraction succeeds on richMKV.
// richMKV carries a subtitle track (ID=3, codec="srt") but no media blocks, so
// the output is an empty SRT file — this tests the no-cue success path.
func TestCmdExtractSubtitle_SRT(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "sub.srt")

	stdout := capture(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "3", "-o", out, "-format", "srt"})
	})
	if !strings.Contains(stdout, "extracted subtitle track") {
		t.Errorf("CmdExtractSubtitle stdout = %q; want 'extracted subtitle track'", stdout)
	}
	// Output file must exist (created even when there are no cues).
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output SRT file missing: %v", err)
	}
}

// TestCmdExtractSubtitle_VTT exercises the extractWebVTT path: the output must
// begin with the mandatory "WEBVTT" header even when no cues are present.
func TestCmdExtractSubtitle_VTT(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "sub.vtt")

	stdout := capture(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "3", "-o", out, "-format", "vtt"})
	})
	if !strings.Contains(stdout, "extracted subtitle") {
		t.Errorf("CmdExtractSubtitle vtt stdout = %q; want 'extracted subtitle'", stdout)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read VTT file: %v", err)
	}
	if !strings.HasPrefix(string(data), "WEBVTT") {
		t.Errorf("VTT file does not start with WEBVTT:\n%q", string(data))
	}
}
