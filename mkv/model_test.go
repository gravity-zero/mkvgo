package mkv

import (
	"bytes"
	"testing"
)

func TestRestoreHeader_NoStripping(t *testing.T) {
	tr := &Track{}
	data := []byte{0x01, 0x02}
	got := tr.RestoreHeader(data)
	if &got[0] != &data[0] {
		t.Error("expected same slice when no header stripping")
	}
}

func TestRestoreHeader_EmptyHeaderStripping(t *testing.T) {
	tr := &Track{HeaderStripping: []byte{}}
	data := []byte{0x01, 0x02}
	got := tr.RestoreHeader(data)
	if &got[0] != &data[0] {
		t.Error("expected same slice when header stripping is empty")
	}
}

func TestRestoreHeader_WithData(t *testing.T) {
	tr := &Track{HeaderStripping: []byte{0xAA, 0xBB}}
	data := []byte{0x01, 0x02, 0x03}
	got := tr.RestoreHeader(data)
	want := []byte{0xAA, 0xBB, 0x01, 0x02, 0x03}
	if !bytes.Equal(got, want) {
		t.Errorf("RestoreHeader = %x, want %x", got, want)
	}
}

func TestRestoreHeader_NilData(t *testing.T) {
	tr := &Track{HeaderStripping: []byte{0xAA}}
	got := tr.RestoreHeader(nil)
	want := []byte{0xAA}
	if !bytes.Equal(got, want) {
		t.Errorf("RestoreHeader(nil) = %x, want %x", got, want)
	}
}

func TestIssue_String(t *testing.T) {
	for _, tt := range []struct {
		issue Issue
		want  string
	}{
		{Issue{SeverityError, "bad track"}, "[error] bad track"},
		{Issue{SeverityWarning, "no cues"}, "[warning] no cues"},
	} {
		if got := tt.issue.String(); got != tt.want {
			t.Errorf("Issue.String() = %q, want %q", got, tt.want)
		}
	}
}

func TestDiff_String(t *testing.T) {
	for _, tt := range []struct {
		diff Diff
		want string
	}{
		{Diff{DiffAdded, "tracks", "track 2"}, "[added] tracks: track 2"},
		{Diff{DiffRemoved, "chapters", "ch 1"}, "[removed] chapters: ch 1"},
		{Diff{DiffChanged, "info", "title"}, "[changed] info: title"},
	} {
		if got := tt.diff.String(); got != tt.want {
			t.Errorf("Diff.String() = %q, want %q", got, tt.want)
		}
	}
}
