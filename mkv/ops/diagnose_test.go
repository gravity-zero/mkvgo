package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func findingKinds(d *Diagnosis) []string {
	kinds := make([]string, 0, len(d.Findings))
	for _, f := range d.Findings {
		kinds = append(kinds, f.Kind)
	}
	return kinds
}

func hasFinding(d *Diagnosis, kind string) *Finding {
	for i := range d.Findings {
		if d.Findings[i].Kind == kind {
			return &d.Findings[i]
		}
	}
	return nil
}

// TestDiagnose_Healthy: a well-formed indexed file with aligned audio yields
// zero findings and no tolerant walk.
func TestDiagnose_Healthy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: []byte{0x01}},
		})
	}
	path := buildMultiClusterMKV(t, dir, "healthy.mkv", tracks, sets, 4000)

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Healthy || len(d.Findings) != 0 {
		t.Errorf("want healthy, got findings %v", findingKinds(d))
	}
	if d.Damage != nil {
		t.Error("a coherent declared size must not trigger the tolerant walk")
	}
	if d.CueHealth == nil || !d.CueHealth.Healthy {
		t.Error("cue health report missing or unhealthy")
	}
	if d.AudioDelaysNs[2] != 0 {
		t.Errorf("aligned audio should read delay 0, got %d", d.AudioDelaysNs[2])
	}
}

// TestDiagnose_NoIndex: an unindexed file is classified with the reindex remedy.
func TestDiagnose_NoIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	path := buildMKVWithCues(t, dir, "nocue.mkv", tracks, cueHealthFixtureSets(3), nil)

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	f := hasFinding(d, "no-index")
	if d.Healthy || f == nil {
		t.Fatalf("want the no-index finding, got %v", findingKinds(d))
	}
	if !strings.Contains(f.Remedy, "reindex") {
		t.Errorf("no-index remedy must name reindex: %q", f.Remedy)
	}
}

// TestDiagnose_AudioDelay: a late audio track yields a per-track finding whose
// remedy is the exact retime invocation.
func TestDiagnose_AudioDelay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}},
		})
	}
	path := buildMultiClusterMKV(t, dir, "late.mkv", tracks, sets, 4000)

	d, err := Diagnose(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	f := hasFinding(d, "audio-delay")
	if d.Healthy || f == nil {
		t.Fatalf("want the audio-delay finding, got %v", findingKinds(d))
	}
	if f.Track != 2 || f.DelayNs != 300_000_000 {
		t.Errorf("finding = %+v, want track 2 at 300ms", f)
	}
	if !strings.Contains(f.Remedy, "retime --shift 2=-300") {
		t.Errorf("remedy must be the exact retime invocation: %q", f.Remedy)
	}
}

// TestDiagnose_Truncated: a cut tail triggers the walk and the re-download
// verdict, with the damage map attached.
func TestDiagnose_Truncated(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "src.mkv", 8)
	data := readAll(t, src)
	offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
	trunc := filepath.Join(dir, "trunc.mkv")
	writeAll(t, trunc, data[:offsets[5]+15])

	d, err := Diagnose(ctx, trunc)
	if err != nil {
		t.Fatal(err)
	}
	f := hasFinding(d, "truncated")
	if d.Healthy || f == nil {
		t.Fatalf("want the truncated finding, got %v", findingKinds(d))
	}
	if !strings.Contains(f.Remedy, "re-download") {
		t.Errorf("a truncated source needs the re-download remedy: %q", f.Remedy)
	}
	if d.Damage == nil || !d.Damage.TruncatedTail {
		t.Error("the damage map with the truncated verdict must be attached")
	}
	// The sizes a caller routes on, stated rather than re-parsed from the
	// header: the declared end is the intact file's size.
	kept, whole := int64(offsets[5]+15), int64(len(data))
	if d.FileSize != kept || d.DeclaredSize != whole || d.MissingTailBytes != whole-kept {
		t.Errorf("sizes = file %d declared %d missing %d, want %d / %d / %d", d.FileSize, d.DeclaredSize, d.MissingTailBytes, kept, whole, whole-kept)
	}
}

// TestDiagnose_TrailingJunk: bytes beyond the declared Segment end are
// reported without being mistaken for corruption - and never for a
// truncated download: surplus bytes are not missing bytes, and the
// truncated verdict's remedy ("re-download") would be wrong for a file
// whose media is complete.
func TestDiagnose_TrailingJunk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "src.mkv", 4)
	data := readAll(t, src)
	junked := filepath.Join(dir, "junk.mkv")
	writeAll(t, junked, append(append([]byte{}, data...), make([]byte, 512)...))

	d, err := Diagnose(ctx, junked)
	if err != nil {
		t.Fatal(err)
	}
	if d.Healthy {
		t.Fatalf("want unhealthy, got healthy with findings %v", findingKinds(d))
	}
	if f := hasFinding(d, "trailing-junk"); f == nil {
		t.Errorf("want the trailing-junk finding, got %v", findingKinds(d))
	}
	if f := hasFinding(d, "truncated"); f != nil {
		t.Errorf("surplus bytes must not diagnose truncated: %v", findingKinds(d))
	}
	if d.Damage == nil || d.Damage.TruncatedTail {
		t.Error("trailing junk must not set the TruncatedTail verdict")
	}
}

// TestDiagnose_WrongContainer: a .mkv whose content is ISO base media is a
// classification, not an error - one structured finding settles the file
// for every future scan pass, where an error would re-enter it each time.
func TestDiagnose_WrongContainer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp4ish := filepath.Join(dir, "fake.mkv")
	writeAll(t, mp4ish, []byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0})

	d, err := Diagnose(ctx, mp4ish)
	if err != nil {
		t.Fatalf("an MP4 disguised as .mkv must classify, not fail: %v", err)
	}
	f := hasFinding(d, "wrong-container")
	if d.Healthy || f == nil {
		t.Fatalf("want the wrong-container finding, got %v", findingKinds(d))
	}
	if !strings.Contains(f.Remedy, "rename") && !strings.Contains(f.Remedy, "remux") {
		t.Errorf("the remedy must point to rename/remux: %q", f.Remedy)
	}

	// Content that is neither Matroska nor ISO base media stays an error:
	// there is nothing to classify about arbitrary garbage.
	garbage := filepath.Join(dir, "garbage.mkv")
	writeAll(t, garbage, []byte{0xAA, 0xBB, 0xCC, 0xDD, 'x', 'y', 'z', 'w'})
	if _, err := Diagnose(ctx, garbage); err == nil {
		t.Error("garbage content must keep failing Diagnose")
	}
}
