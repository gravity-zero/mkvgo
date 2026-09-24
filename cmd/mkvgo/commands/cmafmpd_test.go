package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mp4"
)

const cmafRegfix = "../../../internal/testdata/regfix.mkv"

// TestCLICMAFMPD packages a two-rung ladder with the library, moves each
// rendition into its own directory (the layout an external encoder leaves),
// and checks the command writes one manifest over the four of them.
func TestCLICMAFMPD(t *testing.T) {
	root := t.TempDir()
	ladder := filepath.Join(root, "ladder")
	if err := mp4.RemuxToABR(context.Background(), []string{cmafRegfix, cmafRegfix}, ladder, mp4.Options{SegmentMs: 1000}); err != nil {
		t.Fatalf("RemuxToABR: %v", err)
	}
	move := func(from, prefix, to string) {
		t.Helper()
		if err := os.MkdirAll(to, 0o755); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(from)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, prefix) || !(strings.HasSuffix(n, ".m4s") || strings.HasSuffix(n, ".mp4")) {
				continue
			}
			if err := os.Rename(filepath.Join(from, n), filepath.Join(to, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := filepath.Join(root, "ext")
	move(filepath.Join(ladder, "v1"), "init_a1", filepath.Join(out, "a1"))
	move(filepath.Join(ladder, "v1"), "seg_a1_", filepath.Join(out, "a1"))
	move(filepath.Join(ladder, "v1"), "init_a2", filepath.Join(out, "a2"))
	move(filepath.Join(ladder, "v1"), "seg_a2_", filepath.Join(out, "a2"))
	move(filepath.Join(ladder, "v1"), "init", filepath.Join(out, "hd"))
	move(filepath.Join(ladder, "v1"), "seg", filepath.Join(out, "hd"))
	move(filepath.Join(ladder, "v2"), "init", filepath.Join(out, "sd"))
	move(filepath.Join(ladder, "v2"), "seg", filepath.Join(out, "sd"))

	mpdPath := filepath.Join(out, "manifest.mpd")
	CmdCMAFMPD([]string{"-o", mpdPath, filepath.Join(out, "hd"), filepath.Join(out, "sd"), "--audio", filepath.Join(out, "a1"), "--audio", filepath.Join(out, "a2")})
	data, err := os.ReadFile(mpdPath)
	if err != nil {
		t.Fatal(err)
	}
	mpd := string(data)
	for _, want := range []string{
		`contentType="video"`, `id="v1"`, `id="v2"`, `id="a1"`, `id="a2"`,
		`initialization="hd/init.mp4"`, `media="sd/seg$Number%05d$.m4s"`, `initialization="a1/init_a1.mp4"`,
	} {
		if !strings.Contains(mpd, want) {
			t.Errorf("manifest missing %s:\n%s", want, mpd)
		}
	}
	if n := strings.Count(mpd, "<Representation "); n != 4 {
		t.Errorf("representations = %d, want 4", n)
	}
}

func TestNaturalLess(t *testing.T) {
	want := []string{"seg2.m4s", "seg9.m4s", "seg09.m4s", "seg10.m4s", "seg100.m4s"}
	for i := 1; i < len(want); i++ {
		if !naturalLess(want[i-1], want[i]) || naturalLess(want[i], want[i-1]) {
			t.Errorf("%q must sort before %q", want[i-1], want[i])
		}
	}
	if !naturalLess("a.m4s", "b.m4s") || naturalLess("seg00001.m4s", "seg00001.m4s") {
		t.Error("plain ordering broken")
	}
}
