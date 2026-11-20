package commands

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/ebml"
)

// growSegment adds n to the size a sized Segment declares, in place.
func growSegment(t *testing.T, data []byte, n int64) {
	t.Helper()
	r := bytes.NewReader(data)
	h, hdr, err := ebml.ReadElementHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	segAt := int64(hdr) + h.Size
	if _, err := r.Seek(segAt, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	seg, segHdr, err := ebml.ReadElementHeader(r)
	if err != nil || seg.Size < 0 {
		t.Fatalf("no sized Segment: %v", err)
	}
	var size bytes.Buffer
	if _, err := ebml.WriteDataSizeWidth(&size, seg.Size+n, segHdr-4); err != nil {
		t.Fatal(err)
	}
	copy(data[segAt+4:], size.Bytes())
}

// TestWritingCommandsTellWhatTheSourceWasReadPast: a command that wrote from a source read past stray bytes said
// nothing, and exited 0 under --exit-damaged. It warns, names the rewrite, and counts as damage; a sound source
// stays silent.
func TestWritingCommandsTellWhatTheSourceWasReadPast(t *testing.T) {
	sound := livefixture.Build(livefixture.Options{})
	cluster := bytes.Index(sound, []byte{0x1F, 0x43, 0xB6, 0x75})
	junk := append(append(append([]byte(nil), sound[:cluster]...), []byte("0000\x00\x00")...), sound[cluster:]...)
	tailed := livefixture.Build(livefixture.Options{SizedClusters: true, TailTags: true})
	tags := bytes.LastIndex(tailed, []byte{0x12, 0x54, 0xC3, 0x67})
	tail := append(append(append([]byte(nil), tailed[:tags]...), []byte("0000\x00\x00")...), tailed[tags:]...)
	growSegment(t, tail, 6)
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"stray bytes ahead of the first Cluster": {junk, "6 undecodable byte(s)"},
		"stray bytes ahead of the closing Tags":  {tail, "6 undecodable byte(s)"},
		"sound":                                  {sound, ""},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "src.mkv")
			if err := os.WriteFile(path, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			old, oldSaw := os.Stderr, sawDamage
			os.Stderr, sawDamage = f, false
			tellResynced(path)
			told := sawDamage
			os.Stderr, sawDamage = old, oldSaw
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(f)
			f.Close()
			if tc.want == "" {
				if len(out) != 0 || told {
					t.Errorf("a sound source: %q, damage noted %v", out, told)
				}
				return
			}
			if !strings.Contains(string(out), tc.want) || !strings.Contains(string(out), "mkvgo reindex --resync") || !told {
				t.Errorf("stderr %q, damage noted %v; want the bytes passed over, the rewrite, and damage noted", out, told)
			}
		})
	}
}
