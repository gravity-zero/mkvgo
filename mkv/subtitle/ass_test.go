package subtitle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseASSTimestamp(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"0:00:00.00", 0, true},
		{"0:01:02.34", 62340, true},
		{"1:30:00.00", 5400000, true},
		{"9:59:59.99", 35999990, true},
		{"bad", 0, false},
		{"0:00:00", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseASSTimestamp(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("ParseASSTimestamp(%q) = (%d, %v), want (%d, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFormatASSTimestamp(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "0:00:00.00"},
		{62340, "0:01:02.34"},
		{5400000, "1:30:00.00"},
		{3723450, "1:02:03.45"},
	}
	for _, tt := range tests {
		got := FormatASSTimestamp(tt.ms)
		if got != tt.want {
			t.Errorf("FormatASSTimestamp(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestFormatASSTimestampRoundTrip(t *testing.T) {
	ms := int64(5025120) // must be multiple of 10 for ASS centiseconds
	formatted := FormatASSTimestamp(ms)
	parsed, ok := ParseASSTimestamp(formatted)
	if !ok {
		t.Fatalf("failed to parse %q", formatted)
	}
	if parsed != ms {
		t.Errorf("round-trip: %d -> %q -> %d", ms, formatted, parsed)
	}
}

func TestParseASS(t *testing.T) {
	content := `[Script Info]
Title: Test
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize
Style: Default,Arial,20

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello World
Dialogue: 0,0:00:05.00,0:00:08.00,Default,,0,0,0,,Second line
`

	dir := t.TempDir()
	path := filepath.Join(dir, "test.ass")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	ass, err := ParseASS(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ass.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(ass.Events))
	}
	if ass.Events[0].StartMs != 1000 || ass.Events[0].EndMs != 4000 {
		t.Errorf("event 0 times = %d-%d", ass.Events[0].StartMs, ass.Events[0].EndMs)
	}
	if ass.Header == "" {
		t.Error("header should not be empty")
	}
}

func TestParseASSEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.ass")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	ass, err := ParseASS(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ass.Events) != 0 {
		t.Errorf("got %d events, want 0", len(ass.Events))
	}
}

func TestParseASSNoDialogue(t *testing.T) {
	content := `[Script Info]
Title: Test

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Comment: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,This is a comment
`

	dir := t.TempDir()
	path := filepath.Join(dir, "nodia.ass")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	ass, err := ParseASS(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ass.Events) != 0 {
		t.Errorf("got %d events, want 0", len(ass.Events))
	}
}

func TestParseASSFileNotFound(t *testing.T) {
	_, err := ParseASS("/nonexistent/path/file.ass")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestParseASSMalformedDialogue(t *testing.T) {
	content := `[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,bad_timestamp,0:00:04.00,Default,,0,0,0,,Hello
Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Valid line
`

	dir := t.TempDir()
	path := filepath.Join(dir, "malformed.ass")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	ass, err := ParseASS(path)
	if err != nil {
		t.Fatal(err)
	}
	// malformed line skipped, valid line kept
	if len(ass.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(ass.Events))
	}
}

func TestParseASSLinesBeforeFormat(t *testing.T) {
	content := `[Events]
; comment before format
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,Hello
`
	dir := t.TempDir()
	path := filepath.Join(dir, "preformat.ass")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	ass, err := ParseASS(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ass.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(ass.Events))
	}
}

func TestParseASSNotEnoughFields(t *testing.T) {
	content := `[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:04.00
`

	dir := t.TempDir()
	path := filepath.Join(dir, "short.ass")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	ass, err := ParseASS(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ass.Events) != 0 {
		t.Errorf("got %d events, want 0 (too few fields)", len(ass.Events))
	}
}
