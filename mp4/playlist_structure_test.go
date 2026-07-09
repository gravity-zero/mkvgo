package mp4

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// uriAttrRe extracts a URI="..." attribute value (EXT-X-MEDIA, EXT-X-MAP,
// EXT-X-I-FRAME-STREAM-INF, EXT-X-KEY).
var uriAttrRe = regexp.MustCompile(`URI="([^"]*)"`)

// collectPlaylistRefs returns every resource URI a playlist file references:
// bare segment/playlist lines plus URI="..." attributes. Fragment-only and
// data:/http(s): URIs (a key server, an embedded key) are skipped - they are
// not local files.
func collectPlaylistRefs(t *testing.T, plPath string) []string {
	t.Helper()
	f, err := os.Open(plPath)
	if err != nil {
		t.Fatalf("open %s: %v", plPath, err)
	}
	defer f.Close()
	var refs []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			return
		}
		refs = append(refs, u)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if m := uriAttrRe.FindStringSubmatch(line); m != nil {
				add(m[1])
			}
			continue
		}
		add(line) // a bare line is a segment or variant playlist URI
	}
	return refs
}

// assertNoDanglingRefs walks every .m3u8 in dir (recursively) and fails if any
// referenced resource does not exist on disk. This is the structural guard the
// EXT-X-KEY/EXT-X-MAP ordering bug motivated: a player fails on a playlist that
// points at something that was never written, and a byte-parity or decode test
// never notices because it only looks at what WAS produced.
func assertNoDanglingRefs(t *testing.T, dir string) int {
	t.Helper()
	checked := 0
	err := filepathWalkM3U8(dir, func(pl string) {
		for _, ref := range collectPlaylistRefs(t, pl) {
			target := filepath.Join(filepath.Dir(pl), ref)
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s references %q which does not exist (%v)", rel(dir, pl), ref, err)
			}
			checked++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return checked
}

func filepathWalkM3U8(dir string, fn func(string)) error {
	return filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".m3u8") {
			fn(p)
		}
		return nil
	})
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}

// TestHLSPlaylistsHaveNoDanglingReferences packages a source every way that
// produces playlists and asserts every URI they reference exists on disk.
func TestHLSPlaylistsHaveNoDanglingReferences(t *testing.T) {
	ctx := context.Background()
	src := buildCENCFixture(t) // video + audio, real CodecPrivate

	cases := []struct {
		name string
		run  func(dir string) error
	}{
		{"plain", func(d string) error { return RemuxToHLS(ctx, src, d, Options{SegmentMs: 1000}) }},
		{"encrypted-aes", func(d string) error {
			return RemuxToHLS(ctx, src, d, Options{SegmentMs: 1000, Encrypt: &HLSEncryption{Key: cencKey, KeyURI: "https://example.test/k"}})
		}},
		{"cenc", func(d string) error {
			return RemuxToHLS(ctx, src, d, Options{SegmentMs: 1000, CENC: &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIVFor("cenc"), KeyURI: "https://example.test/k"}})
		}},
		{"abr", func(d string) error { return RemuxToABR(ctx, []string{src, src}, d, Options{SegmentMs: 1000}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := c.run(dir); err != nil {
				t.Fatal(err)
			}
			if n := assertNoDanglingRefs(t, dir); n == 0 {
				t.Fatal("no playlist references were checked")
			}
		})
	}
}

// TestHLSPlaylistVersionCoversFeatures asserts EXT-X-VERSION is high enough for
// the tags each playlist uses: EXT-X-MAP requires version >= 6, and an IV on
// EXT-X-KEY requires version >= 2 (RFC 8216). Declaring too low a version makes
// a strict player reject the playlist.
func TestHLSPlaylistVersionCoversFeatures(t *testing.T) {
	ctx := context.Background()
	src := buildCENCFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 1000, Encrypt: &HLSEncryption{Key: cencKey, IV: make([]byte, 16), KeyURI: "https://example.test/k"}}); err != nil {
		t.Fatal(err)
	}
	filepathWalkM3U8(dir, func(pl string) {
		b, err := os.ReadFile(pl)
		if err != nil {
			t.Fatal(err)
		}
		ver := playlistVersion(b)
		if bytes.Contains(b, []byte("#EXT-X-MAP")) && ver < 6 {
			t.Errorf("%s uses EXT-X-MAP but declares EXT-X-VERSION:%d (< 6)", rel(dir, pl), ver)
		}
		if bytes.Contains(b, []byte("#EXT-X-KEY")) && bytes.Contains(b, []byte(",IV=")) && ver < 2 {
			t.Errorf("%s uses EXT-X-KEY with IV but declares EXT-X-VERSION:%d (< 2)", rel(dir, pl), ver)
		}
	})
}

var versionRe = regexp.MustCompile(`#EXT-X-VERSION:(\d+)`)

func playlistVersion(b []byte) int {
	if m := versionRe.FindSubmatch(b); m != nil {
		v := 0
		for _, c := range m[1] {
			v = v*10 + int(c-'0')
		}
		return v
	}
	return 1 // absent means version 1
}
