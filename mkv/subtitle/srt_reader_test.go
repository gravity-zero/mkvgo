package subtitle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestParseSRTReaderMatchesParseSRT: ParseSRT opened the file itself, so a caller reading through its own filesystem
// (a remote source) could not use it; ParseSRTReader parses a reader the caller opened, entry for entry the same.
func TestParseSRTReaderMatchesParseSRT(t *testing.T) {
	const srt = "1\n00:00:01,000 --> 00:00:02,500\nHello\nworld\n\n2\n00:00:03,000 --> 00:00:04,000\nBye\n"
	path := filepath.Join(t.TempDir(), "a.srt")
	if err := os.WriteFile(path, []byte(srt), 0o644); err != nil {
		t.Fatal(err)
	}
	fromFile, err := ParseSRT(path)
	if err != nil {
		t.Fatal(err)
	}
	fromReader, err := ParseSRTReader(strings.NewReader(srt))
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile) != 2 || !reflect.DeepEqual(fromFile, fromReader) {
		t.Errorf("ParseSRT %+v, ParseSRTReader %+v; want the same two entries", fromFile, fromReader)
	}
}
