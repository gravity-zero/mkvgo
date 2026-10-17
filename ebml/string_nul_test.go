package ebml

import (
	"bytes"
	"testing"
)

// TestReadStringStopsAtTheFirstNull: RFC 8794 section 13 - the bytes after a string's first null are not part of it.
func TestReadStringStopsAtTheFirstNull(t *testing.T) {
	for in, want := range map[string]string{"eng\x00xx": "eng", "eng\x00\x00": "eng", "\x00abc": "", "fre": "fre"} {
		got, err := ReadString(bytes.NewReader([]byte(in)), int64(len(in)))
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
}
