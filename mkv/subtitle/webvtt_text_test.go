package subtitle

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriteWebVTTKeepsACueOneCue: a blank line or an arrow in a cue's text neither ends the cue nor starts another.
func TestWriteWebVTTKeepsACueOneCue(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteWebVTT(&buf, []Cue{{StartMs: 0, EndMs: 1000, Text: "a\r\n\r\nb --> c\n\nd"}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "-->"); n != 1 {
		t.Errorf("%d arrows, want the timing line's only:\n%s", n, out)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(out, "WEBVTT\n\n"), "\n\n")
	if strings.Contains(body, "\n\n") || strings.Contains(body, "\r") {
		t.Errorf("the cue is cut by a blank line or a carriage return:\n%q", body)
	}
	for _, w := range []string{"a", "b --&gt; c", "d"} {
		if !strings.Contains(body, w) {
			t.Errorf("the cue lost %q:\n%s", w, body)
		}
	}
}
