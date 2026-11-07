package ops

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
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

// TestDiagnose_AudioDelayOrder: several late audio tracks are reported in track
// order on every call, so two scans of the same file compare equal.
func TestDiagnose_AudioDelayOrder(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2), audioTrack(3), audioTrack(4), audioTrack(5)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		set := []mkv.Block{{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}}}
		for n := uint64(2); n <= 5; n++ {
			set = append(set, mkv.Block{TrackNumber: n, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}})
		}
		sets = append(sets, set)
	}
	path := buildMultiClusterMKV(t, dir, "late4.mkv", tracks, sets, 4000)

	for run := 0; run < 20; run++ {
		d, err := Diagnose(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		var got []uint64
		for _, f := range d.Findings {
			if f.Kind == "audio-delay" {
				got = append(got, f.Track)
			}
		}
		if want := []uint64{2, 3, 4, 5}; !slices.Equal(got, want) {
			t.Fatalf("run %d: audio-delay findings on tracks %v, want %v", run, got, want)
		}
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

// TestDiagnose_TruncatedCleanCut: a file cut exactly between two clusters is
// as incomplete as one cut inside a block - the declared size says bytes are
// missing - and is reported truncated, never healthy; a cut inside a block is
// the same cut tail, not damage with media continuing behind it.
func TestDiagnose_TruncatedCleanCut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 8)
	for i := range 8 {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: make([]byte, 64)},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: make([]byte, 16)},
		})
	}
	sources := map[string]string{
		"unindexed": salvageFixture(t, dir, "src.mkv", 8),
		"indexed":   filepath.Join(dir, "front.mkv"),
	}
	// Reindexed: the Cues ahead of the clusters survive the cut, so the tail
	// walk from the index runs over the cut tail.
	if err := Reindex(ctx, buildMultiClusterMKV(t, dir, "cued.mkv", tracks, sets, 8000), sources["indexed"]); err != nil {
		t.Fatal(err)
	}
	for src, path := range sources {
		data := readAll(t, path)
		offsets := findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
		for cutName, cut := range map[string]int64{"between clusters": offsets[5], "inside a block": offsets[5] + 15} {
			name := src + ", " + cutName
			trunc := filepath.Join(dir, "trunc.mkv")
			writeAll(t, trunc, data[:cut])
			d, err := Diagnose(ctx, trunc)
			if err != nil {
				t.Fatal(err)
			}
			if d.Healthy || hasFinding(d, "truncated") == nil {
				t.Errorf("%s: want the truncated finding, got %v", name, findingKinds(d))
			}
			if f := hasFinding(d, "damaged"); f != nil {
				t.Errorf("%s: the cut tail is not damage inside the file: %q", name, f.Detail)
			}
		}
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

// TestDiagnose_TrailingJunkIsNoDamage: bytes after the Segment's declared end, zeros or not, are trailing junk and
// nothing else: the walks opened at a cue stop at the Segment's end instead of passing them over as damage inside the
// file, which made a sound file read as damaged and named reindex --resync.
func TestDiagnose_TrailingJunkIsNoDamage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := readAll(t, salvageFixture(t, dir, "src.mkv", 4))
	pattern := make([]byte, 8192)
	for i := range pattern {
		pattern[i] = byte(i*7 + 3)
	}
	for name, junk := range map[string][]byte{"zeros": make([]byte, 512), "bytes": pattern} {
		junked := filepath.Join(dir, name+".mkv")
		writeAll(t, junked, append(append([]byte{}, data...), junk...))
		d, err := Diagnose(ctx, junked)
		if err != nil {
			t.Fatal(err)
		}
		if hasFinding(d, "trailing-junk") == nil || hasFinding(d, "damaged") != nil {
			t.Errorf("%s: findings %v, want trailing-junk and no damaged", name, findingKinds(d))
		}
		if d.TrackEnds != nil && d.TrackEnds.SkippedBytes != 0 {
			t.Errorf("%s: the tail walk passed over %d bytes as damage", name, d.TrackEnds.SkippedBytes)
		}
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

// TestDiagnoseStreamedSizeIsANote: a whole file whose Segment declares no size
// (a streamed write) is healthy; the unknown size is a note, not a finding.
func TestDiagnoseStreamedSizeIsANote(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 4)
	for i := range 4 {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: make([]byte, 64)},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: make([]byte, 16)},
		})
	}
	sealed := filepath.Join(dir, "sealed.mkv")
	if err := Reindex(ctx, buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 4000), sealed); err != nil {
		t.Fatal(err)
	}
	streamed := filepath.Join(dir, "streamed.mkv")
	writeAll(t, streamed, makeSegmentStreamed(t, readAll(t, sealed)))
	d, err := Diagnose(ctx, streamed)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Healthy || len(d.Findings) != 0 {
		t.Errorf("streamed whole file: healthy %v, findings %+v; want healthy, none", d.Healthy, d.Findings)
	}
	if len(d.Notes) != 1 || d.Notes[0].Kind != mkv.FindingStreamedSize {
		t.Errorf("streamed whole file: notes %+v, want one %s", d.Notes, mkv.FindingStreamedSize)
	}
}

// streamedFixture is a sealed file of 8 one-second clusters, video and audio, rewritten with a Segment of unknown
// size; its bytes and the offsets of its clusters.
func streamedFixture(t *testing.T, dir string) ([]byte, []int64) {
	t.Helper()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2)}
	sets := make([][]mkv.Block, 0, 8)
	for i := range 8 {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: bytes.Repeat([]byte{0x11}, 400)},
			{TrackNumber: 2, Timecode: ts, Keyframe: true, Data: bytes.Repeat([]byte{0x22}, 100)},
		})
	}
	sealed := filepath.Join(dir, "sealed.mkv")
	if err := Reindex(context.Background(), buildMultiClusterMKV(t, dir, "src.mkv", tracks, sets, 8000), sealed); err != nil {
		t.Fatal(err)
	}
	data := makeSegmentStreamed(t, readAll(t, sealed))
	return data, findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})
}

// TestDiagnoseStreamedCutInsideABlock: a Segment of unknown size that stops inside a block is cut short - the walk
// stopping inside the media says so, there being no declared size to compare with - a truncated finding at the tail.
func TestDiagnoseStreamedCutInsideABlock(t *testing.T) {
	dir := t.TempDir()
	data, _ := streamedFixture(t, dir)
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data[:len(data)-50])
	d, err := Diagnose(context.Background(), cut)
	if err != nil {
		t.Fatal(err)
	}
	f := hasFinding(d, mkv.FindingTruncated)
	if d.Healthy || f == nil || f.Scope != mkv.ScopeTail {
		t.Fatalf("streamed file cut in a block: healthy %v, findings %+v; want a truncated finding at the tail", d.Healthy, d.Findings)
	}
	if hasFinding(d, mkv.FindingDamaged) != nil {
		t.Errorf("the cut tail is reported as damage too: %+v", d.Findings)
	}
}

// TestDiagnoseDeepVerifyStreamed: under DeepVerify a Segment of unknown size runs to the end of the file - nothing of
// it is trailing junk - and damage inside its body is reported.
func TestDiagnoseDeepVerifyStreamed(t *testing.T) {
	dir := t.TempDir()
	data, clusters := streamedFixture(t, dir)
	whole := filepath.Join(dir, "whole.mkv")
	writeAll(t, whole, data)
	d, err := Diagnose(context.Background(), whole, mkv.Options{DeepVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Healthy || len(d.Findings) != 0 {
		t.Errorf("whole streamed file under DeepVerify: healthy %v, findings %+v; want healthy, none", d.Healthy, d.Findings)
	}
	damaged := append([]byte{}, data...)
	copy(damaged[clusters[3]:], bytes.Repeat([]byte{0xFF}, 64)) // the fourth cluster's header and first block
	bad := filepath.Join(dir, "damaged.mkv")
	writeAll(t, bad, damaged)
	if d, err = Diagnose(context.Background(), bad, mkv.Options{DeepVerify: true}); err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, mkv.FindingDamaged); f == nil || f.Scope != mkv.ScopeBody {
		t.Errorf("streamed file damaged in its body under DeepVerify: findings %+v; want damaged in the body", d.Findings)
	} else if !strings.Contains(f.Detail, "skipped") || !strings.Contains(f.Detail, "media included") || !strings.Contains(f.Detail, "mkvgo salvage --dry-run") {
		// The count is what the walk jumped to the next Cluster it could read, not the damage itself.
		t.Errorf("damaged detail %q: want the bytes skipped, media included, and where the damage is mapped", f.Detail)
	}
	if hasFinding(d, mkv.FindingTrailingJunk) != nil {
		t.Errorf("a Segment of unknown size has no trailing junk: %+v", d.Findings)
	}
}

// TestDiagnoseDeepVerifyStreamedCut: the symmetric case of TestDiagnoseDeepVerifyStreamed - a Segment of unknown size
// cut inside a block under DeepVerify reported its cut tail as body damage, not as a truncation.
func TestDiagnoseDeepVerifyStreamedCut(t *testing.T) {
	dir := t.TempDir()
	data, _ := streamedFixture(t, dir)
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data[:len(data)-50])
	d, err := Diagnose(context.Background(), cut, mkv.Options{DeepVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if f := hasFinding(d, mkv.FindingTruncated); d.Healthy || f == nil || f.Scope != mkv.ScopeTail {
		t.Errorf("streamed file cut in a block under DeepVerify: healthy %v, findings %+v; want truncated at the tail", d.Healthy, d.Findings)
	}
	if hasFinding(d, mkv.FindingDamaged) != nil {
		t.Errorf("the cut tail is reported as damage too: %+v", d.Findings)
	}
}
