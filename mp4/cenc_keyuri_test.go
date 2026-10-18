package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCENCKeyURIRequired pins that the content key reaches a playlist only on InlineKey.
func TestCENCKeyURIRequired(t *testing.T) {
	src := buildCENCFixture(t)
	bare := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8}
	err := RemuxToHLS(context.Background(), src, t.TempDir(), Options{SegmentMs: 1000, CENC: bare})
	if err == nil || !strings.Contains(err.Error(), "KeyURI") || !strings.Contains(err.Error(), "InlineKey") {
		t.Fatalf("CENC without KeyURI: err = %v, want a refusal naming KeyURI and InlineKey", err)
	}
	dir := t.TempDir()
	inline := &CENCOptions{Scheme: "cenc", Key: cencKey, KeyID: cencKID, IV: cencIV8, InlineKey: true}
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, CENC: inline}); err != nil {
		t.Fatal(err)
	}
	pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pl, []byte(`URI="data:application/octet-stream;base64,`)) {
		t.Errorf("InlineKey playlist has no data: key URI:\n%s", pl)
	}
}
