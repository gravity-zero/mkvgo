package mp4

// packaging_notcov_test.go closes mutation-testing NOT COVERED gaps across
// the packaging/output files (hlspack.go, concatplan.go, chaptermarkers.go,
// abr.go, hls.go, concat.go, singlefile.go, cenc.go, moov.go): branches
// reached only under a specific option, track shape or MP4-source input that
// no existing test builds. Helpers are prefixed pkgcov to avoid clashing with
// the house fixtures (buildMKV, buildConcatSource, buildABRVariant, cencKey,
// makeAC3, ...) they reuse throughout.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// --- hlspack.go: MP4-source packaging (collectFromMP4/mp4PlanSamples/mp4SubCues) ---

// pkgcovMP4SourceWithSubAndAC3 builds an MKV with an h264 video track, an
// AC-3 audio track (needsFirstFrame: its MP4 sample entry is built lazily
// from the first frame, never from CodecPrivate) and an SRT subtitle track,
// then remuxes it to MP4. RemuxToHLS/PlanHLS run against the resulting MP4
// path exercise the MP4-source packaging code in hlspack.go
// (collectFromMP4, mp4PlanSamples, mp4SubCues) - never reached when the
// packaging source is Matroska itself.
func pkgcovMP4SourceWithSubAndAC3(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	ch := uint8(6)
	sr := 48000.0
	frame := makeAC3(0 /*48k*/, 20, 8, 1, 7, 1)
	var gblocks []genBlock
	for i := 0; i < 60; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 75; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 32, key: true, data: frame})
	}
	gblocks = append(gblocks, genBlock{track: 3, pts: 500, key: true, data: []byte("cue one")})
	gblocks = append(gblocks, genBlock{track: 3, pts: 1500, key: true, data: []byte("cue two")})
	sortGenBlocks(gblocks)
	mkvSrc := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "ac3", Channels: &ch, SampleRate: &sr},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, gblocks)
	mp4Src := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, mp4Src, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	return mp4Src
}

// TestPkgcovMP4SourceRemuxToHLS_SubtitleAndLazyAC3Entry pins hlspack.go's
// collectFromMP4: the subtitle-routing branch (read the sample, decode it,
// resolve cue.EndMs from a positive durMs) and the lazy sample-entry branch
// (built from the first AC-3 sample, since AC-3 has no CodecPrivate).
func TestPkgcovMP4SourceRemuxToHLS_SubtitleAndLazyAC3Entry(t *testing.T) {
	mp4Src := pkgcovMP4SourceWithSubAndAC3(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), mp4Src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	vtt, err := os.ReadFile(filepath.Join(dir, "sub1_00001.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(vtt, []byte("cue one")) {
		t.Errorf("subtitle cue missing from the MP4-source collect path:\n%s", vtt)
	}
	// cue.EndMs = ctsMs (500) + durMs (1000, clamped to the gap before "cue
	// two" at 1500) = 1500: exercises hlspack.go's durMs>0 EndMs branch with a
	// value a wrong (or skipped) computation would not produce.
	if !bytes.Contains(vtt, []byte("00:00:00.500 --> 00:00:01.500")) {
		t.Errorf("subtitle cue has the wrong resolved end time:\n%s", vtt)
	}
	audioInit, err := os.ReadFile(filepath.Join(dir, "init_a1.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(audioInit, []byte("ac-3")) || !bytes.Contains(audioInit, []byte("dac3")) {
		t.Errorf("audio init lacks ac-3/dac3 (lazy MP4-source sample entry not built):\n%v", audioInit)
	}
}

// TestPkgcovMP4SourcePlanHLS_SubCuesAndLazyAC3Entry pins hlspack.go's
// mp4PlanSamples (lazy sample entry from the first frame) and mp4SubCues
// (subtitle sample decode + cue.EndMs), the on-demand-plan counterparts of
// the full-pass branches above.
func TestPkgcovMP4SourcePlanHLS_SubCuesAndLazyAC3Entry(t *testing.T) {
	mp4Src := pkgcovMP4SourceWithSubAndAC3(t)
	plan, err := PlanHLS(context.Background(), mp4Src, Options{SegmentMs: 1000})
	if err != nil {
		t.Fatal(err)
	}
	audioInit, _, err := plan.Resource(context.Background(), "init_a1.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(audioInit, []byte("ac-3")) || !bytes.Contains(audioInit, []byte("dac3")) {
		t.Errorf("plan audio init lacks ac-3/dac3 (lazy MP4-source sample entry not built):\n%v", audioInit)
	}
	vtt, err := plan.subtitleSegment(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(vtt, []byte("cue one")) {
		t.Errorf("plan subtitle segment missing the cue text (mp4SubCues not run):\n%s", vtt)
	}
}

// --- concatplan.go: PlanConcat/ConcatPlan error paths ------------------------

// TestPkgcovPlanConcatProbeFailureWraps pins the probeConcatSource error wrap
// in PlanConcat (concatplan.go): a source that fails to open must be named in
// the returned error.
func TestPkgcovPlanConcatProbeFailureWraps(t *testing.T) {
	good := buildConcatSource(t, 1000, "eng", "cue", 200)
	missing := filepath.Join(t.TempDir(), "missing.mkv")
	_, err := PlanConcat(context.Background(), []string{good, missing}, Options{SegmentMs: 500})
	if err == nil {
		t.Fatal("expected an error for a missing second source")
	}
	if !strings.Contains(err.Error(), "part 2") {
		t.Errorf("error must name the failing part: %v", err)
	}
}

// pkgcovNoCuesMKV builds a valid Matroska file (Tracks + clusters, sample
// table intact) whose writer never appends a CuePoint, so the resulting file
// carries no Cues element at all. probeConcatSource (track/codec metadata
// only) accepts it; PlanHLS itself refuses it ("no Cues index").
func pkgcovNoCuesMKV(t *testing.T, tracks []mkv.Track, blocks []genBlock) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nocues.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 1_000_000
	var blks []mkv.Block
	var durationMs int64
	for _, gb := range blocks {
		blks = append(blks, mkv.Block{TrackNumber: gb.track, Timecode: gb.pts, Keyframe: gb.key, Data: gb.data})
		if gb.pts > durationMs {
			durationMs = gb.pts
		}
	}
	c := &mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: scale, MuxingApp: "mkvgo-test", WritingApp: "mkvgo-test"}}
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := m.WriteMetadata(c, tracks, durationMs); err != nil {
		f.Close()
		t.Fatal(err)
	}
	// writer.WriteCluster (not MKVWriter.WriteClusterWithCues) writes valid
	// clusters without ever appending to m.Cues, so Finalize below skips the
	// Cues element entirely (len(m.Cues) == 0).
	start := 0
	for i := 1; i <= len(blks); i++ {
		if i == len(blks) || blks[i].Timecode-blks[start].Timecode >= 1000 {
			if err := writer.WriteCluster(m.W, blks[start].Timecode, scale, blks[start:i]); err != nil {
				f.Close()
				t.Fatal(err)
			}
			start = i
		}
	}
	if err := m.Finalize(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPkgcovPlanConcatPlanHLSFailureWraps pins the PlanHLS error wrap in
// PlanConcat (concatplan.go): a source that passes the compatibility probe
// but fails PlanHLS itself (no Cues index) must be named in the returned
// error.
func TestPkgcovPlanConcatPlanHLSFailureWraps(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 50; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	videoOnly := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}
	part1 := buildMKV(t, videoOnly, gblocks)
	part2 := pkgcovNoCuesMKV(t, videoOnly, gblocks)

	_, err := PlanConcat(context.Background(), []string{part1, part2}, Options{SegmentMs: 500})
	if err == nil {
		t.Fatal("expected an error: part 2 has no Cues index")
	}
	if !strings.Contains(err.Error(), "part 2") || !strings.Contains(err.Error(), "no Cues index") {
		t.Errorf("error must wrap part 2's no-Cues PlanHLS failure: %v", err)
	}
}

// TestPkgcovConcatPlanSubtitleMisalignedErrors pins ConcatPlan.windowedSubtitle
// and .wholeSubtitle's !subsOK guard (concatplan.go): with mismatched subtitle
// layouts across parts, both must refuse rather than serve a wrong/absent
// rendition.
func TestPkgcovConcatPlanSubtitleMisalignedErrors(t *testing.T) {
	src0 := buildConcatSource(t, 1000, "eng", "cue-a", 200)
	src1 := buildConcatSourceNoSubs(t, 1000)
	cp, err := PlanConcat(context.Background(), []string{src0, src1}, Options{SegmentMs: 500})
	if err != nil {
		t.Fatal(err)
	}
	if cp.subsOK {
		t.Fatal("expected misaligned subtitle layouts (subsOK=false) for this fixture")
	}
	if _, err := cp.wholeSubtitle(context.Background(), 0); err == nil {
		t.Error("wholeSubtitle must refuse when subsOK is false")
	}
	if _, err := cp.windowedSubtitle(context.Background(), 0, 0, 0); err == nil {
		t.Error("windowedSubtitle must refuse when subsOK is false")
	}
}

// TestPkgcovConcatPlanWindowedSubtitleOutOfRange pins windowedSubtitle's
// rendition-index and segment-index range checks (concatplan.go), reachable
// only once subsOK is true (an aligned subtitle layout across parts).
func TestPkgcovConcatPlanWindowedSubtitleOutOfRange(t *testing.T) {
	src0 := buildConcatSource(t, 1000, "eng", "cue-a", 200)
	src1 := buildConcatSource(t, 1000, "eng", "cue-b", 200)
	cp, err := PlanConcat(context.Background(), []string{src0, src1}, Options{SegmentMs: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !cp.subsOK {
		t.Fatal("expected aligned subtitle layouts (subsOK=true) for this fixture")
	}
	if _, err := cp.windowedSubtitle(context.Background(), 0, 5, 0); err == nil {
		t.Error("an out-of-range subtitle rendition index must error")
	}
	if _, err := cp.windowedSubtitle(context.Background(), 0, 0, 9999); err == nil {
		t.Error("an out-of-range subtitle segment index must error")
	}
}

// --- chaptermarkers.go: explicit ChapterTimeEnd is kept, not overwritten ----

// TestPkgcovChapterMarkersExplicitEndKept pins chapterMarkers' "already has an
// explicit end" branch (chaptermarkers.go): a chapter whose EndMs is already
// > StartMs must keep it, rather than being overwritten by the next
// chapter's start time.
func TestPkgcovChapterMarkersExplicitEndKept(t *testing.T) {
	chapters := []mkv.Chapter{
		{StartMs: 0, EndMs: 1500, Title: "Explicit"},
		{StartMs: 3000, Title: "NoExplicit"},
	}
	src := chapterFixture(t, chapters)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000, ChapterMarkers: true}); err != nil {
		t.Fatal(err)
	}
	pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pl, []byte("DURATION=1.500")) {
		t.Errorf("explicit ChapterTimeEnd (1500ms) was not kept:\n%s", pl)
	}
	if bytes.Contains(pl, []byte("DURATION=3.000")) {
		t.Errorf("explicit ChapterTimeEnd got overwritten by the next chapter's start:\n%s", pl)
	}
}

// --- abr.go: reference-variant subtitle group (master + combined DASH) -----

// pkgcovABRVariantWithSub builds an h264+aac+srt MKV (unnamed, unlabelled
// subtitle track, so the master/DASH subtitle name falls back to "Subtitles
// 1"), for exercising RemuxToABR's reference-variant subtitle handling.
func pkgcovABRVariantWithSub(t *testing.T) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 60; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: cencVideoSample()})
	}
	for i := 0; i < 120; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: cencAudioSample(i)})
	}
	gblocks = append(gblocks, genBlock{track: 3, pts: 500, key: true, data: []byte("only cue")})
	sortGenBlocks(gblocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt"},
	}, gblocks)
}

// TestPkgcovABRMasterAndDASHSubtitleGroup pins buildABRMaster's and
// combinedDASH's subtitle-rendition loops (abr.go): both never run without a
// subtitle track on the ABR reference (v1) variant.
func TestPkgcovABRMasterAndDASHSubtitleGroup(t *testing.T) {
	src := pkgcovABRVariantWithSub(t)
	dir := t.TempDir()
	// The same source twice: trivially segment-aligned, so the combined DASH
	// manifest (which needs abrVariantsAligned) is emitted too.
	if err := RemuxToABR(context.Background(), []string{src, src}, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	master := readTextFile(t, filepath.Join(dir, "master.m3u8"))
	mustContain(t, master, `TYPE=SUBTITLES,GROUP-ID="subs",NAME="Subtitles 1",AUTOSELECT=YES`)
	mustContain(t, master, `SUBTITLES="subs"`)

	mpd := readTextFile(t, filepath.Join(dir, "manifest.mpd"))
	mustContain(t, mpd, `mimeType="text/vtt" contentType="text"`)
	mustContain(t, mpd, `<Representation id="sub1" bandwidth="0">`)
	mustContain(t, mpd, `<BaseURL>v1/sub1.vtt</BaseURL>`)
}

// TestPkgcovRemuxToABRVariantFailureWraps pins RemuxToABR's per-variant error
// wrap (abr.go): a variant that fails to package must be named in the
// returned error.
func TestPkgcovRemuxToABRVariantFailureWraps(t *testing.T) {
	w, h := uint32(320), uint32(240)
	good := buildABRVariant(t, mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h})
	missing := filepath.Join(t.TempDir(), "missing.mkv")
	err := RemuxToABR(context.Background(), []string{good, missing}, t.TempDir(), Options{SegmentMs: 1000})
	if err == nil {
		t.Fatal("expected an error for a missing second variant")
	}
	if !strings.Contains(err.Error(), "variant 2") {
		t.Errorf("error must name the failing variant: %v", err)
	}
}

// --- hls.go: MKV-source collectFragSamples (Progress, lazy AC-3 entry), segEndOrLast ---

// TestPkgcovRemuxToHLSProgressMKVSource pins collectFragSamples' progress
// wiring (hls.go): Options.Progress must be invoked, with the total equal to
// the source file's size, when packaging from a Matroska source.
func TestPkgcovRemuxToHLSProgressMKVSource(t *testing.T) {
	w, h := uint32(320), uint32(240)
	var gblocks []genBlock
	for i := 0; i < 60; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}}, gblocks)

	var calls int
	var lastTotal int64
	dir := t.TempDir()
	err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, Progress: func(processed, total int64) {
		calls++
		lastTotal = total
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("Progress callback never invoked for a Matroska source (collectFragSamples)")
	}
	info, statErr := os.Stat(src)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if lastTotal != info.Size() {
		t.Errorf("progress total = %d, want the source file size %d", lastTotal, info.Size())
	}
}

// TestPkgcovRemuxToHLSMKVSourceLazyAC3SampleEntry pins collectFragSamples'
// lazy sample-entry branch (hls.go) for a Matroska (not MP4) source: AC-3 has
// no CodecPrivate, so its MP4 sample entry is built from the first buffered
// sample.
func TestPkgcovRemuxToHLSMKVSourceLazyAC3SampleEntry(t *testing.T) {
	w, h := uint32(320), uint32(240)
	ch := uint8(6)
	sr := 48000.0
	frame := makeAC3(0, 20, 8, 1, 7, 1)
	var gblocks []genBlock
	for i := 0; i < 60; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 40; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 32, key: true, data: frame})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "ac3", Channels: &ch, SampleRate: &sr},
	}, gblocks)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	audioInit, err := os.ReadFile(filepath.Join(dir, "init_a1.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(audioInit, []byte("ac-3")) || !bytes.Contains(audioInit, []byte("dac3")) {
		t.Errorf("audio init lacks ac-3/dac3 (lazy sample entry not built from a Matroska source):\n%v", audioInit)
	}
}

// TestPkgcovSegEndOrLastEmptySamplesFallback pins segEndOrLast's defensive
// empty-track fallback (hls.go): every real caller has already rejected a
// zero-sample track earlier in the pipeline, so this is exercised directly.
func TestPkgcovSegEndOrLastEmptySamplesFallback(t *testing.T) {
	fts := []*fragTrack{{outTrack: &outTrack{}}} // no samples; primaryIndex falls back to 0
	bounds := []int64{0, 2000, 4000}
	got := segEndOrLast(bounds, len(bounds)-1, fts)
	want := bounds[len(bounds)-1]
	if got != want {
		t.Errorf("segEndOrLast with zero samples = %d, want the fallback %d (last bound)", got, want)
	}
}

// --- concat.go: validateConcatCompat audio mismatches, probe error wrap ----

// pkgcovConcatSourceAudioLangs builds a video + 2-audio-track (lang1, lang2)
// MKV, for validateConcatCompat's per-audio-slot comparison.
func pkgcovConcatSourceAudioLangs(t *testing.T, durMs int64, lang1, lang2 string) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	var gblocks []genBlock
	nv := int(durMs / 40)
	for i := 0; i < nv; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	na := int(durMs / 20)
	for i := 0; i < na; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
		gblocks = append(gblocks, genBlock{track: 3, pts: int64(i) * 20, key: true, data: []byte{0xBB, byte(i)}})
	}
	sortGenBlocks(gblocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: lang1},
		{ID: 3, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: lang2},
	}, gblocks)
}

// pkgcovConcatSourceOneAudio builds a video + single-audio-track MKV, for
// validateConcatCompat's audio-count mismatch.
func pkgcovConcatSourceOneAudio(t *testing.T, durMs int64) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr := 48000.0
	ch := uint8(2)
	var gblocks []genBlock
	nv := int(durMs / 40)
	for i := 0; i < nv; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	na := int(durMs / 20)
	for i := 0; i < na; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch, Language: "eng"},
	}, gblocks)
}

// TestPkgcovConcatAudioCountMismatch pins validateConcatCompat's audio-count
// mismatch message (concat.go).
func TestPkgcovConcatAudioCountMismatch(t *testing.T) {
	part1 := pkgcovConcatSourceAudioLangs(t, 1000, "eng", "fre")
	part2 := pkgcovConcatSourceOneAudio(t, 1000)
	err := RemuxConcatToHLS(context.Background(), []string{part1, part2}, t.TempDir(), Options{SegmentMs: 500})
	if err == nil {
		t.Fatal("expected an incompatibility error (2 vs 1 audio tracks)")
	}
	if !strings.Contains(err.Error(), "has 1 audio track(s), part 1 has 2") {
		t.Errorf("error should report the audio-count mismatch: %v", err)
	}
}

// TestPkgcovConcatAudioLangMismatch pins validateConcatCompat's per-slot
// audio codec/language mismatch message (concat.go).
func TestPkgcovConcatAudioLangMismatch(t *testing.T) {
	part1 := pkgcovConcatSourceAudioLangs(t, 1000, "eng", "fre")
	part2 := pkgcovConcatSourceAudioLangs(t, 1000, "eng", "spa")
	err := RemuxConcatToHLS(context.Background(), []string{part1, part2}, t.TempDir(), Options{SegmentMs: 500})
	if err == nil {
		t.Fatal("expected an incompatibility error (audio 2 language differs)")
	}
	if !strings.Contains(err.Error(), "audio 2 is aac/spa, part 1's is aac/fre") {
		t.Errorf("error should report the audio codec/lang mismatch: %v", err)
	}
}

// TestPkgcovRemuxConcatToHLSProbeFailureWraps pins the probeConcatSource error
// wrap in RemuxConcatToHLS (concat.go): a source that fails to open must be
// named in the returned error.
func TestPkgcovRemuxConcatToHLSProbeFailureWraps(t *testing.T) {
	good := pkgcovConcatSourceOneAudio(t, 1000)
	missing := filepath.Join(t.TempDir(), "missing.mkv")
	err := RemuxConcatToHLS(context.Background(), []string{good, missing}, t.TempDir(), Options{SegmentMs: 500})
	if err == nil {
		t.Fatal("expected an error for a missing second source")
	}
	if !strings.Contains(err.Error(), "part 2") {
		t.Errorf("error must name the failing part: %v", err)
	}
}

// --- singlefile.go: SingleFile DASH manifest subtitle AdaptationSet --------

// TestPkgcovSingleFileDASHSubtitleAdaptationSet pins
// buildDASHManifestSingle's subtitle-rendition loop (singlefile.go), never
// run without a subtitle track in a SingleFile presentation.
func TestPkgcovSingleFileDASHSubtitleAdaptationSet(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 60; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 120; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	gblocks = append(gblocks, genBlock{track: 3, pts: 500, key: true, data: []byte("only cue")})
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt", Language: "fre"},
	}, gblocks)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000, SingleFile: true}); err != nil {
		t.Fatal(err)
	}
	mpd, err := os.ReadFile(filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(mpd), `mimeType="text/vtt" contentType="text"`)
	mustContain(t, string(mpd), ` lang="fre"`)
	mustContain(t, string(mpd), `<Representation id="sub1" bandwidth="0">`)
	mustContain(t, string(mpd), `<BaseURL>sub1.vtt</BaseURL>`)
}

// --- cenc.go: splitNALSubsamples clamp, cbcsPatternEncrypt crypt-run clamp ---

// TestPkgcovSplitNALSubsamplesEmptyNALClearClamp pins the "clear region wider
// than the whole NAL" clamp (cenc.go): a bare zero-length NAL (a 4-byte
// length prefix with no payload, no NAL header byte to include) has clear =
// 4 + nalHeaderLen naturally exceed 4 + n and must clamp down to 4 + n.
func TestPkgcovSplitNALSubsamplesEmptyNALClearClamp(t *testing.T) {
	data := []byte{0, 0, 0, 0} // one NAL, length=0, no payload
	subs, err := splitNALSubsamples(data, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d subsamples, want 1", len(subs))
	}
	if subs[0].clear != 4 || subs[0].protected != 0 {
		t.Errorf("clear=%d protected=%d, want clear=4 protected=0 (clamped: no NAL header to include)",
			subs[0].clear, subs[0].protected)
	}
}

// TestPkgcovCbcsPatternEncryptCryptRunClamp pins the crypt-run clamp in
// cbcsPatternEncrypt (cenc.go): requesting more crypt blocks than the region
// actually holds must encrypt exactly what is there, not read/write past it.
func TestPkgcovCbcsPatternEncryptCryptRunClamp(t *testing.T) {
	block, err := aes.NewCipher(cencKey)
	if err != nil {
		t.Fatal(err)
	}
	region := make([]byte, 32) // exactly 2 AES blocks
	for i := range region {
		region[i] = byte(i)
	}
	got := append([]byte(nil), region...)
	// cryptBlocks=3 (48 bytes) requests more than the 32-byte region carries.
	cbcsPatternEncrypt(block, cencIV16, got, 3, 0)

	want := append([]byte(nil), region...)
	cipher.NewCBCEncrypter(block, cencIV16).CryptBlocks(want, want)
	if !bytes.Equal(got, want) {
		t.Errorf("clamped crypt run = %x, want a plain whole-region CBC encrypt %x", got, want)
	}
}

// --- moov.go: multi-track content-hash freeform atoms, sorted by track ID ---

// TestPkgcovContentHashesMultiTrackSortOrder pins the sort.Slice comparator
// in buildMovieMeta (moov.go), only invoked with 2+ content-hash entries: the
// CONTENT_SHA256_N freeform atoms must land in ascending track-ID order.
func TestPkgcovContentHashesMultiTrackSortOrder(t *testing.T) {
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 30; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 60; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, gblocks)

	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(ctx, src, out, Options{ContentHashes: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	i1 := bytes.Index(raw, []byte("CONTENT_SHA256_1"))
	i2 := bytes.Index(raw, []byte("CONTENT_SHA256_2"))
	if i1 < 0 || i2 < 0 {
		t.Fatalf("expected both CONTENT_SHA256_1 and CONTENT_SHA256_2 atoms, got i1=%d i2=%d", i1, i2)
	}
	if i1 > i2 {
		t.Errorf("hash atoms out of ascending track-ID order: track 1's atom at %d, track 2's at %d", i1, i2)
	}

	mismatches, err := VerifyContentHashes(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 0 {
		t.Errorf("unexpected content-hash mismatches: %v", mismatches)
	}
}
