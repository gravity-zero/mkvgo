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

// TestHashRoutesByContent: hash picks its engine from the first bytes, never the extension.
func TestHashRoutesByContent(t *testing.T) {
	dir := t.TempDir()
	copyAs := func(src, name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "testdata", src))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, name)
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	for _, stride := range []struct {
		name           string
		prefix, stride int
	}{{"TS", 0, 188}, {"M2TS", 4, 192}} {
		b := make([]byte, 8*stride.stride)
		for i := 0; i < 8; i++ {
			b[i*stride.stride+stride.prefix] = 0x47
		}
		for _, ext := range []string{".mp4", ".mkv", ".ts"} {
			p := filepath.Join(dir, stride.name+ext)
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
			msg := fatalMessage(t, func() { commands.CmdHash([]string{p}) })
			if !strings.Contains(msg, "MPEG-TS detected") {
				t.Errorf("hash %s%s: %q, want the MPEG-TS refusal", stride.name, ext, msg)
			}
			if got, _ := os.ReadFile(p); !bytes.Equal(got, b) {
				t.Errorf("hash %s%s modified the refused file", stride.name, ext)
			}
		}
	}

	mkvAsMP4 := copyAs("sample.mkv", "matroska.mp4")
	hashed := filepath.Join(dir, "hashed.mkv")
	commands.CmdHash([]string{"-o", hashed, mkvAsMP4})
	mismatches, err := matroska.VerifyContentHashes(context.Background(), hashed)
	if err != nil || len(mismatches) != 0 {
		t.Errorf("a Matroska named .mp4 was not hashed: %v %v", mismatches, err)
	}

	mp4AsMKV := copyAs("fps30.mp4", "isobmff.mkv")
	before, _ := os.ReadFile(mp4AsMKV)
	msg := fatalMessage(t, func() { commands.CmdHash([]string{mp4AsMKV}) })
	if !strings.Contains(msg, "to-mp4 --hash") {
		t.Errorf("an MP4 named .mkv: %q, want the MP4 answer (to-mp4 --hash)", msg)
	}
	if after, _ := os.ReadFile(mp4AsMKV); !bytes.Equal(before, after) {
		t.Error("hash modified an MP4 named .mkv")
	}
}
