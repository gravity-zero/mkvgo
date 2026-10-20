package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mp4"
)

// pastSegmentJunk is what a sound file may carry after its sealed Segment: none of it is damage.
var pastSegmentJunk = map[string][]byte{
	"one byte":           {0x00},
	"short of a header":  {0x1F, 0x43, 0xB6},
	"a valid element":    {0x1F, 0x43, 0xB6, 0x75, 0x83, 0xE7, 0x81, 0x00},
	"an invalid element": {0x1F, 0x43, 0xB6, 0x75, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0xA3, 0x85},
	"zeros":              make([]byte, 512),
	"bytes":              junkPattern(8192),
}

func junkPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

// soundFiles are the files known sound: the Matroska test corpus, a muxed fixture and a live recording (unknown-size
// Segment and Clusters, ended by the end of the file).
func soundFiles(t *testing.T, dir string) []string {
	t.Helper()
	live := filepath.Join(dir, "live.mkv")
	writeAll(t, live, livefixture.Build(livefixture.Options{}))
	files := []string{salvageFixture(t, dir, "muxed.mkv", 4), live}
	for _, pat := range []string{"*.mkv", "*.webm"} {
		m, err := filepath.Glob(filepath.Join("..", "..", "internal", "testdata", pat))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	return files
}

// damageView is everything that would call a file damaged: what the gate holds at zero on a sound file.
type damageView struct {
	resynced   [3]int64 // Read, ReadMeta, ReadStream
	segmentEnd int64
	validate   []string
	findings   []string
	skipped    int64
	plan       map[string][]byte
	planErr    error
	stats      mp4.HLSPlanStats
}

func viewDamage(t *testing.T, path string) damageView {
	t.Helper()
	ctx := context.Background()
	var v damageView
	c, err := reader.Open(ctx, path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	v.resynced[0], v.segmentEnd = c.ResyncedBytes, c.SegmentEnd
	if m, err := reader.OpenMeta(ctx, path); err == nil {
		v.resynced[1] = m.ResyncedBytes
	} else {
		t.Errorf("%s: OpenMeta: %v", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, _, err := reader.ReadStream(ctx, f); err == nil {
		v.resynced[2] = s.ResyncedBytes
	} else {
		t.Errorf("%s: ReadStream: %v", path, err)
	}
	f.Close()
	issues, err := Validate(ctx, path)
	if err != nil {
		t.Fatalf("%s: Validate: %v", path, err)
	}
	for _, is := range issues {
		v.validate = append(v.validate, is.Code)
	}
	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatalf("%s: Diagnose: %v", path, err)
	}
	v.findings = findingKinds(d)
	if d.TrackEnds != nil {
		v.skipped = d.TrackEnds.SkippedBytes
	}
	p, err := mp4.PlanHLS(ctx, path, mp4.Options{SegmentMs: 2000, DamageBytes: -1})
	if v.planErr = err; err == nil {
		v.plan = map[string][]byte{}
		for _, name := range p.Resources() {
			b, _, err := p.Resource(ctx, name)
			if err != nil {
				t.Errorf("%s: %s: %v", path, name, err)
			}
			v.plan[name] = b
		}
		v.stats = p.Stats()
	}
	return v
}

func hasDamage(v damageView) []string {
	var why []string
	if v.resynced != [3]int64{} {
		why = append(why, "resynced bytes")
	}
	if slices.Contains(v.validate, "undecodable-bytes") || slices.Contains(v.validate, "cluster-read-error") || slices.Contains(v.validate, "clusters-unreadable") {
		why = append(why, "Validate damage")
	}
	if slices.Contains(v.findings, "damaged") || slices.Contains(v.findings, "truncated") {
		why = append(why, "Diagnose damage")
	}
	if v.skipped != 0 {
		why = append(why, "tail walk skipped bytes")
	}
	if v.stats.DamageSkips != 0 || v.stats.DamageRefusals != 0 {
		why = append(why, "HLS plan damage")
	}
	return why
}

// TestSoundFilesShowNoDamage: no check calls a sound file damaged, with or without junk past its sealed Segment, and
// the junk changes nothing an HLS plan serves (its master's BANDWIDTH included).
func TestSoundFilesShowNoDamage(t *testing.T) {
	dir := t.TempDir()
	for _, src := range soundFiles(t, dir) {
		clean := viewDamage(t, src)
		if why := hasDamage(clean); len(why) > 0 {
			t.Errorf("%s (sound): %v (validate %v, findings %v)", filepath.Base(src), why, clean.validate, clean.findings)
		}
		if clean.planErr != nil {
			t.Logf("%s: no HLS plan: %v", filepath.Base(src), clean.planErr)
		}
		if clean.segmentEnd == 0 {
			t.Logf("%s: Segment of unknown size, no junk variant", filepath.Base(src))
			continue
		}
		data := readAll(t, src)
		for name, junk := range pastSegmentJunk {
			junked := filepath.Join(dir, filepath.Base(src)+"."+name)
			writeAll(t, junked, append(append([]byte{}, data...), junk...))
			v := viewDamage(t, junked)
			if why := hasDamage(v); len(why) > 0 {
				t.Errorf("%s + %s: %v (validate %v, findings %v)", filepath.Base(src), name, why, v.validate, v.findings)
			}
			if !slices.Equal(v.validate, clean.validate) {
				t.Errorf("%s + %s: Validate %v, sound %v", filepath.Base(src), name, v.validate, clean.validate)
			}
			if !slices.Contains(v.findings, "trailing-junk") {
				t.Errorf("%s + %s: findings %v, want trailing-junk", filepath.Base(src), name, v.findings)
			}
			if (v.planErr == nil) != (clean.planErr == nil) {
				t.Errorf("%s + %s: plan error %v, sound %v", filepath.Base(src), name, v.planErr, clean.planErr)
			}
			for res, want := range clean.plan {
				if !bytes.Equal(v.plan[res], want) {
					t.Errorf("%s + %s: %s differs from the sound file's (%d bytes, sound %d)", filepath.Base(src), name, res, len(v.plan[res]), len(want))
				}
			}
		}
	}
}
