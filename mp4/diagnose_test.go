package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func findKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

func diagKinds(t *testing.T, path string) []string {
	t.Helper()
	d, err := Diagnose(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(d.Findings))
	for _, f := range d.Findings {
		kinds = append(kinds, f.Kind)
	}
	return kinds
}

// TestMP4Diagnose_HealthyAndDelay: a clean file is healthy; after a retime
// delay the triage reports the exact per-track finding with the retime
// remedy - the two ops closing the loop on each other.
func TestMP4Diagnose_HealthyAndDelay(t *testing.T) {
	ctx := context.Background()
	path := editShiftFixture(t, false)

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Healthy || len(d.Findings) != 0 {
		t.Fatalf("clean file must be healthy, got %+v", d.Findings)
	}
	if d.AudioDelaysNs[2] != 0 {
		t.Errorf("aligned audio should read delay 0, got %d", d.AudioDelaysNs[2])
	}
	if d.CueHealth != nil || d.Damage != nil {
		t.Error("MP4 triage must not carry Matroska-only sections")
	}

	if err := RetimeTracks(ctx, path, map[uint64]int64{2: 300_000_000}); err != nil {
		t.Fatal(err)
	}
	d, err = Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Healthy || len(d.Findings) != 1 || d.Findings[0].Kind != "audio-delay" {
		t.Fatalf("delayed audio must yield the audio-delay finding, got %+v", d.Findings)
	}
	f := d.Findings[0]
	if f.Track != 2 || f.DelayNs != 300_000_000 || !strings.Contains(f.Remedy, "retime --shift 2=-300") {
		t.Errorf("finding = %+v, want track 2 at 300ms with the exact retime invocation", f)
	}
	// Apply the remedy: the triage goes back to healthy.
	if err := RetimeTracks(ctx, path, map[uint64]int64{2: -300_000_000}); err != nil {
		t.Fatal(err)
	}
	if kinds := diagKinds(t, path); len(kinds) != 0 {
		t.Errorf("after the remedy the file must be healthy again, got %v", kinds)
	}
}

// TestMP4Diagnose_TruncatedNoMoovJunk: the three structural verdicts.
func TestMP4Diagnose_TruncatedNoMoovJunk(t *testing.T) {
	base := editShiftFixture(t, false) // moov at the tail
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// Cut mid-mdat: a declared box overruns the file -> truncated, and the
	// tail moov is gone with it -> no-moov rides along.
	trunc := filepath.Join(dir, "trunc.mp4")
	if err := os.WriteFile(trunc, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	kinds := diagKinds(t, trunc)
	if !findKind(kinds, "truncated") || !findKind(kinds, "no-moov") {
		t.Errorf("a mid-mdat cut must report truncated + no-moov, got %v", kinds)
	}

	// moov retired without a replacement -> no-moov alone.
	noMoov := append([]byte(nil), data...)
	at := bytes.LastIndex(noMoov, []byte("moov"))
	copy(noMoov[at:], "free")
	noMoovPath := filepath.Join(dir, "nomoov.mp4")
	if err := os.WriteFile(noMoovPath, noMoov, 0o644); err != nil {
		t.Fatal(err)
	}
	if kinds := diagKinds(t, noMoovPath); !findKind(kinds, "no-moov") {
		t.Errorf("a moov-less file must report no-moov, got %v", kinds)
	}

	// Garbage appended after the last box -> trailing-junk. (Garbage whose
	// first bytes HAPPEN to read as a plausible box header is reported as
	// "truncated" instead - indistinguishable from a cut-off real box.)
	junk := filepath.Join(dir, "junk.mp4")
	garbage := []byte{0x00, 0x00, 0x00, 0x10, 0x01, 0x02, 0x03, 0x04, 0xDE, 0xAD, 0xBE, 0xEF}
	if err := os.WriteFile(junk, append(append([]byte(nil), data...), garbage...), 0o644); err != nil {
		t.Fatal(err)
	}
	if kinds := diagKinds(t, junk); !findKind(kinds, "trailing-junk") {
		t.Errorf("appended garbage must report trailing-junk, got %v", kinds)
	}
}
