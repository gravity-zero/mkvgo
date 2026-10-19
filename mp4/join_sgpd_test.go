package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A join of files whose sample group descriptions differ drops the groups, whose indices would point into another file's table, and keeps every sample as a join of files that agree does.
func TestJoinDropsSampleGroupsTheFilesDescribeApart(t *testing.T) {
	a := copyFixture(t, "bframes_aac.mp4")
	data, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte("sgpd\x01\x00\x00\x00roll"))
	if i < 4 {
		t.Fatal("the fixture holds no roll sample group description")
	}
	mut := bytes.Clone(data)
	mut[i-4+int(binary.BigEndian.Uint32(data[i-4:]))-1] = 0xFE // roll_distance -1 becomes -2
	dir := t.TempDir()
	b := filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(b, mut, 0o644); err != nil {
		t.Fatal(err)
	}
	same, apart := filepath.Join(dir, "same.mp4"), filepath.Join(dir, "apart.mp4")
	if err := Join(context.Background(), []string{a, a}, same); err != nil {
		t.Fatal(err)
	}
	if err := Join(context.Background(), []string{a, b}, apart); err != nil {
		t.Fatal(err)
	}
	sd, _ := os.ReadFile(same)
	ad, _ := os.ReadFile(apart)
	if !bytes.Contains(sd, []byte("sgpd")) || !bytes.Contains(sd, []byte("sbgp")) {
		t.Error("a join of files that agree lost their sample groups")
	}
	if bytes.Contains(ad, []byte("sgpd")) || bytes.Contains(ad, []byte("sbgp")) {
		t.Error("a join of files that disagree kept sample groups")
	}
	group := regexp.MustCompile(`/roll=\d+`)
	ws, gs := sampleSeqs(t, same), sampleSeqs(t, apart)
	if len(ws) != len(gs) {
		t.Fatalf("%d tracks, want %d", len(gs), len(ws))
	}
	grouped := false
	for ti := range ws {
		w := strings.Join(ws[ti], ",")
		grouped = grouped || group.MatchString(w)
		if g := strings.Join(gs[ti], ","); group.ReplaceAllString(w, "") != g {
			t.Errorf("track %d: the samples differ from a join of files that agree", ti+1)
		}
	}
	if !grouped {
		t.Error("the oracle sees no sample in a roll group")
	}
}
