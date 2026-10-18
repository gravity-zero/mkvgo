package mp4

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewrittenURIsStayInTheirAttribute pins that a rewritten URI holding a quote, an ampersand, a line break or an
// accent neither adds playlist lines nor breaks the DASH manifest.
func TestRewrittenURIsStayInTheirAttribute(t *testing.T) {
	src := buildCENCFixture(t)
	dir := t.TempDir()
	rw := func(name string) string { return "é/" + name + "?t=a\"b&c\n#EXT-X-ENDLIST" }
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, RewriteURL: rw}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"master.m3u8", "playlist.m3u8", "audio1.m3u8"} {
		pl, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		ends := 0
		for _, line := range strings.Split(string(pl), "\n") {
			if strings.HasPrefix(line, "#EXT-X-ENDLIST") {
				ends++
			}
			if strings.Contains(line, "URI=") && strings.Count(line[strings.Index(line, "URI="):], `"`)%2 != 0 {
				t.Errorf("%s: a quote closed a URI early: %s", name, line)
			}
		}
		if ends > 1 || (ends == 1 && !bytes.HasSuffix(bytes.TrimSpace(pl), []byte("#EXT-X-ENDLIST"))) {
			t.Errorf("%s: a URI added playlist lines:\n%s", name, pl)
		}
		for _, l := range strings.Split(string(pl), "\n") {
			if strings.Contains(l, `\u00`) || strings.Contains(l, `\"`) {
				t.Errorf("%s: a URI was Go-quoted: %s", name, l)
			}
		}
	}
	mpd, err := os.ReadFile(filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(bytes.NewReader(mpd))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("manifest.mpd is not XML: %v\n%s", err, mpd)
		}
	}
}
