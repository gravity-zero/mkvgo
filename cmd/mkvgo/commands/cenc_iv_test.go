package commands_test

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
)

// stderrOf runs fn and returns what it wrote to stderr.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

var generatedIV = regexp.MustCompile(`--cenc-iv not given: generated ([0-9a-f]+)\.`)

func TestCENCIVGeneratedWhenNotGiven(t *testing.T) {
	for _, c := range []struct {
		scheme string
		digits int
	}{{"cenc", 16}, {"cbcs", 32}} {
		t.Run(c.scheme, func(t *testing.T) {
			common := []string{"--cenc-scheme", c.scheme, "--cenc-key", sec06Key, "--cenc-kid", "ffeeddccbbaa99887766554433221100", "--cenc-key-uri", "skd://k"}
			var first map[string][]byte
			msg := stderrOf(t, func() { first = hlsDir(t, common...) })
			m := generatedIV.FindStringSubmatch(msg)
			if m == nil || len(m[1]) != c.digits {
				t.Fatalf("no generated IV of %d hex digits on stderr: %q", c.digits, msg)
			}
			var again map[string][]byte
			if msg := stderrOf(t, func() { again = hlsDir(t, append(common, "--cenc-iv", m[1])...) }); strings.Contains(msg, "generated") {
				t.Fatalf("an IV generated although --cenc-iv was given: %q", msg)
			}
			sameFiles(t, first, again)
			var other map[string][]byte
			if m2 := generatedIV.FindStringSubmatch(stderrOf(t, func() { other = hlsDir(t, common...) })); m2 == nil || m2[1] == m[1] {
				t.Fatalf("two runs generated the same IV %v", m2)
			}
			same := 0
			for name, b := range other {
				if string(b) == string(first[name]) {
					same++
				}
			}
			if same == len(first) {
				t.Fatal("two runs with different IVs wrote the same files")
			}
		})
	}
}

func TestCENCIVRequiredOnDemand(t *testing.T) {
	src := "../../../internal/testdata/sample.mkv"
	cenc := []string{"--cenc-scheme", "cbcs", "--cenc-key", sec06Key, "--cenc-kid", "ffeeddccbbaa99887766554433221100", "--cenc-key-uri", "skd://k"}
	out := filepath.Join(t.TempDir(), "seg.m4s")
	for name, run := range map[string]func(){
		"hls-segment":    func() { commands.CmdHLSSegment(append([]string{"-o", out, src, "1"}, cenc...)) },
		"abr-segment":    func() { commands.CmdABRSegment(append([]string{"-o", out, "master.m3u8", src, src}, cenc...)) },
		"concat-segment": func() { commands.CmdConcatSegment(append([]string{"-o", out, "master.m3u8", src, src}, cenc...)) },
	} {
		if msg := fatalMessage(t, run); !strings.Contains(msg, "--cenc-iv is required") {
			t.Fatalf("%s: %q", name, msg)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a resource written without its IV")
	}
}
