package subtitle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSRTTimestamp(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"00:00:00,000", 0, true},
		{"00:01:02,345", 62345, true},
		{"01:30:00,000", 5400000, true},
		{"99:59:59,999", 359999999, true},
		{"bad", 0, false},
		{"00:00:00", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := ParseSRTTimestamp(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Errorf("ParseSRTTimestamp(%q) = (%d, %v), want (%d, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFormatSRTTime(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "00:00:00,000"},
		{62345, "00:01:02,345"},
		{5400000, "01:30:00,000"},
		{3723456, "01:02:03,456"},
	}
	for _, tt := range tests {
		got := FormatSRTTime(tt.ms)
		if got != tt.want {
			t.Errorf("FormatSRTTime(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestFormatSRTTimeRoundTrip(t *testing.T) {
	ms := int64(5025123)
	formatted := FormatSRTTime(ms)
	parsed, ok := ParseSRTTimestamp(formatted)
	if !ok {
		t.Fatalf("failed to parse %q", formatted)
	}
	if parsed != ms {
		t.Errorf("round-trip: %d -> %q -> %d", ms, formatted, parsed)
	}
}

func TestParseSRT(t *testing.T) {
	content := "1\n00:00:01,000 --> 00:00:04,000\nHello World\n\n2\n00:00:05,000 --> 00:00:08,000\nSecond line\nWith continuation\n\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "test.srt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := ParseSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Text != "Hello World" {
		t.Errorf("entry 0 text = %q", entries[0].Text)
	}
	if entries[0].StartMs != 1000 || entries[0].EndMs != 4000 {
		t.Errorf("entry 0 times = %d-%d", entries[0].StartMs, entries[0].EndMs)
	}
	if entries[1].Text != "Second line\nWith continuation" {
		t.Errorf("entry 1 text = %q", entries[1].Text)
	}
}

func TestParseSRTEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.srt")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := ParseSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestParseSRTMalformed(t *testing.T) {
	content := "not a valid srt\njust random text\n\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.srt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := ParseSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries for malformed SRT, want 0", len(entries))
	}
}

func TestParseSRTWithBOM(t *testing.T) {
	bom := "\xEF\xBB\xBF"
	content := bom + "1\n00:00:01,000 --> 00:00:04,000\nHello\n\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.srt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := ParseSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
}

func TestParseSRTFileNotFound(t *testing.T) {
	_, err := ParseSRT("/nonexistent/path/file.srt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestIsDigitsOnly(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"123", true},
		{"0", true},
		{"", false},
		{"12a", false},
		{" ", false},
	}
	for _, tt := range tests {
		got := isDigitsOnly(tt.s)
		if got != tt.want {
			t.Errorf("isDigitsOnly(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
