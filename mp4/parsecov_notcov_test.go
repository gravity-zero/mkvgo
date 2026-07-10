package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// parsecov_notcov_test.go closes mutation-testing gaps in the box parsers/muxers
// (parse.go, fragparse.go, fragment.go, mux.go, sampletable.go, inband_colour.go)
// left after the earlier mutation-survivor passes: box-header variants an
// existing fixture never produces (64-bit largesize, size==0 "to EOF", co64 vs
// stco, tfra/sidx version 1), and a few last-sample/last-chapter arithmetic
// branches whose exact numeric result was never asserted precisely enough to
// kill a boundary mutant.

// --- parse.go: lazyMoov.parse box-header variants (:395,398,401,404) ----------

// TestLazyMoovParse64BitAndSizeZeroBoxes kills the lazyMoov.parse survivors on
// the 64-bit largesize box (case 1: the truncation check, the ensure, and the
// largesize read) and the size==0 "fills to the parse window end" box (case 0).
// iterBoxes already exercises the same box-header shapes (parse_mut_test.go);
// lazyMoov.parse has its own independent copy of this switch that no test had
// driven with anything but ordinary 32-bit-size boxes.
func TestLazyMoovParse64BitAndSizeZeroBoxes(t *testing.T) {
	// A well-formed 64-bit box: size==1, then an 8-byte largesize giving a total
	// of 24 bytes (16-byte header + 8-byte payload).
	var w bw
	w.u32(1)
	w.fourcc("free")
	w.u64(24)
	w.zeros(8)
	raw64 := w.b

	m := &lazyMoov{r: bytes.NewReader(raw64), buf: make([]byte, len(raw64))}
	if err := m.parse(0, len(raw64)); err != nil {
		t.Fatalf("well-formed 64-bit box: %v", err)
	}
	if m.filled != len(raw64) {
		t.Errorf("filled = %d, want %d (the 64-bit box must be read in full)", m.filled, len(raw64))
	}
	if string(m.buf[4:8]) != "free" {
		t.Errorf("buf = %q, want the real disk bytes read into it", m.buf)
	}

	// One byte short of the 16-byte 64-bit header: must be rejected as truncated,
	// not read past the parse window.
	truncated := raw64[:15]
	m2 := &lazyMoov{r: bytes.NewReader(truncated), buf: make([]byte, len(truncated))}
	if err := m2.parse(0, len(truncated)); err == nil || !strings.Contains(err.Error(), "truncated 64-bit box") {
		t.Errorf("truncated 64-bit header: err = %v, want a truncated-64-bit-box error", err)
	}

	// size==0: the box fills to the end of the parse window; its payload (here
	// distinctive tail bytes) must be read, not skipped as zero.
	tail := []byte("REST-OF-MOOV-BYTES-0123456789")
	var w2 bw
	w2.u32(0)
	w2.fourcc("skip")
	w2.bytes(tail)
	raw0 := w2.b

	m3 := &lazyMoov{r: bytes.NewReader(raw0), buf: make([]byte, len(raw0))}
	if err := m3.parse(0, len(raw0)); err != nil {
		t.Fatalf("size-0 box: %v", err)
	}
	if m3.filled != len(raw0) {
		t.Errorf("filled = %d, want %d (a size-0 box must fill to the parse window end)", m3.filled, len(raw0))
	}
	if !bytes.Equal(m3.buf[8:], tail) {
		t.Errorf("buf tail = %q, want %q (size-0 box body must be the real disk bytes)", m3.buf[8:], tail)
	}
}

// --- parse.go: parseMP4's sidx fallback when no mfra is present (:466) -------

// TestParseMP4SidxKeyframeFallbackWhenNoMfra proves the composed keyframe-source
// logic in parseMP4 itself (as opposed to unit-testing readFragmentKeyframes and
// readSidxKeyframes in isolation, which existing tests already do): a fragmented
// MP4 with no mfra at the tail but a sidx before the first fragment must still
// end up with the video track's keyframesMs populated, via the sidx fallback.
// This needs a genuinely fragmented moov (empty sample tables, as a real init
// segment carries) rather than a progressive moov with an mvex sibling bolted
// on: the latter's trak still carries a real stss/stts, so buildKeyframeTimes
// would already populate keyframesMs before the fallback is even considered.
func TestParseMP4SidxKeyframeFallbackWhenNoMfra(t *testing.T) {
	src, _ := buildLacedFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	initData, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	sidx := box("sidx", sidxPayload(1, 1000, 0, []sidxEntry{
		{dur: 1000, sap: true, sapType: 1},
		{dur: 1000, sap: true, sapType: 1},
	}))
	// The init segment alone has no fragments (no moof/mdat), so appending the
	// sidx right after it still leaves it "before the first fragment" — exactly
	// where readSidxKeyframes looks — and there is no mfra at the tail for
	// readFragmentKeyframes to find.
	file := append(append([]byte{}, initData...), sidx...)
	fsize := int64(len(file))

	mv, err := parseMP4(bytes.NewReader(file), fsize, sampleKeyframes)
	if err != nil {
		t.Fatalf("parseMP4: %v", err)
	}
	if !mv.fragmented {
		t.Fatal("a real init segment's mvex must mark the movie fragmented")
	}
	vt := videoTrack(mv)
	if vt == nil {
		t.Fatal("no video track survived parsing")
	}
	want := []int64{0, 1000}
	if !reflect.DeepEqual(vt.keyframesMs, want) {
		t.Errorf("keyframes = %v, want %v (the sidx fallback must run when readFragmentKeyframes finds no mfra)", vt.keyframesMs, want)
	}
}

// --- parse.go: parseTfra version 1 (:593) -------------------------------------

// TestParseTfraVersion1 kills the version-1 (64-bit time field) survivor in
// parseTfra: every existing tfra fixture (tfraPayload) is version 0.
func TestParseTfraVersion1(t *testing.T) {
	var w bw
	w.u8(1) // version 1: time and moof_offset are 8 bytes each
	w.u24(0)
	w.u32(7) // track_ID
	w.u32(0) // reserved + 1-byte traf/trun/sample id fields
	w.u32(2) // number_of_entries
	for _, tk := range []uint64{1000, 5000} {
		w.u64(tk) // time
		w.u64(0)  // moof_offset
		w.u8(1)
		w.u8(1)
		w.u8(1) // traf_number, trun_number, sample_number
	}

	tid, times, ok := parseTfra(w.b)
	if !ok || tid != 7 {
		t.Fatalf("parseTfra(version 1) = trackID %d ok %v, want 7 true", tid, ok)
	}
	if want := []uint64{1000, 5000}; !reflect.DeepEqual(times, want) {
		t.Errorf("times = %v, want %v (version-1 8-byte time field)", times, want)
	}
}

// --- parse.go: scanSidxBoxes box-header variants (:647,653) -------------------

// TestScanSidxBoxes64BitAndSizeZero kills the scanSidxBoxes survivors on the
// 64-bit largesize sidx box (case 1) and the size==0 "to EOF" sidx box (case
// 0): every existing sidx fixture (box()) uses an ordinary 32-bit size.
func TestScanSidxBoxes64BitAndSizeZero(t *testing.T) {
	payload := sidxPayload(1, 1000, 0, []sidxEntry{{dur: 500, sap: true, sapType: 1}})

	var w64 bw
	w64.u32(1)
	w64.fourcc("sidx")
	w64.u64(uint64(16 + len(payload)))
	w64.bytes(payload)
	file64 := w64.b

	got64 := scanSidxBoxes(bytes.NewReader(file64), int64(len(file64)))
	if len(got64) != 1 || !bytes.Equal(got64[0], payload) {
		t.Errorf("64-bit sidx box: got %d payload(s), want 1 matching payload", len(got64))
	}

	var w0 bw
	w0.u32(0)
	w0.fourcc("sidx")
	w0.bytes(payload)
	file0 := w0.b

	got0 := scanSidxBoxes(bytes.NewReader(file0), int64(len(file0)))
	if len(got0) != 1 || !bytes.Equal(got0[0], payload) {
		t.Errorf("size-0 sidx box: got %d payload(s), want 1 matching payload", len(got0))
	}
}

// --- parse.go: sidxKeyframeMs version 1 (:694,697) ----------------------------

// parsecovSidxV1Payload builds a version-1 sidx payload (8-byte
// earliest_presentation_time/first_offset), which sidxPayload (version 0 only)
// cannot produce.
func parsecovSidxV1Payload(refID, timescale uint32, earliest uint64, entries []sidxEntry) []byte {
	var w bw
	w.u8(1)
	w.u24(0)
	w.u32(refID)
	w.u32(timescale)
	w.u64(earliest) // earliest_presentation_time (v1)
	w.u64(0)        // first_offset (v1)
	w.u16(0)
	w.u16(uint16(len(entries)))
	for _, e := range entries {
		w.u32(0)
		w.u32(e.dur)
		v := e.sapDelta & 0x0FFFFFFF
		if e.sap {
			v |= 1 << 31
		}
		v |= uint32(e.sapType) << 28
		w.u32(v)
	}
	return w.b
}

func TestSidxKeyframeMsVersion1(t *testing.T) {
	p := parsecovSidxV1Payload(1, 1000, 5000, []sidxEntry{
		{dur: 2000, sap: true, sapType: 1},
		{dur: 2000, sap: true, sapType: 1},
	})
	got, ok := sidxKeyframeMs(p, 1)
	if !ok {
		t.Fatal("sidxKeyframeMs(version 1) not ok")
	}
	if want := []int64{5000, 7000}; !reflect.DeepEqual(got, want) {
		t.Errorf("times = %v, want %v (version-1 8-byte earliest_presentation_time/first_offset)", got, want)
	}

	// One byte short of the version-1 16-byte earliest/first_offset fields.
	short := p[:12+15]
	if _, ok := sidxKeyframeMs(short, 1); ok {
		t.Error("a truncated version-1 earliest_presentation_time/first_offset must report not-ok")
	}
}

// --- parse.go: fragmented frame rate from mvex/trex (:796) --------------------

// TestParseMoovFragmentedFrameRateFromTrex kills the survivor on the fragmented
// frame-rate fallback: mkvgo's own fragment writer always declares
// default_sample_duration==0 (real durations ride the trun), so no test built
// from mkvgo's own output ever exercises this interop path (reading a
// third-party fragmented MP4 whose trex does declare a real default). Reuses a
// real video trak (from an actual HLS init segment) and supplies a hand-built
// mvex/trex with a nonzero default so only that value is under test.
func TestParseMoovFragmentedFrameRateFromTrex(t *testing.T) {
	src, _ := buildLacedFixture(t)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 1000}); err != nil {
		t.Fatal(err)
	}
	initData, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	moovPayload, err := readMoov(bytes.NewReader(initData), int64(len(initData)))
	if err != nil {
		t.Fatalf("readMoov: %v", err)
	}
	moovBoxes, err := iterBoxes(moovPayload)
	if err != nil {
		t.Fatalf("iterBoxes: %v", err)
	}
	var mvhd, videoTrak []byte
	for _, b := range moovBoxes {
		switch b.typ {
		case "mvhd":
			mvhd = box("mvhd", b.payload)
		case "trak":
			trakBoxes, err := iterBoxes(b.payload)
			if err != nil {
				t.Fatalf("iterBoxes(trak): %v", err)
			}
			tkhd, ok := findMemBox(trakBoxes, "tkhd")
			if ok && tkhdTrackID(tkhd.payload) == 1 {
				videoTrak = box("trak", b.payload)
			}
		}
	}
	if mvhd == nil || videoTrak == nil {
		t.Fatal("could not find mvhd/video trak in the real init segment")
	}

	// A hand-built mvex/trex declaring a real (nonzero) default_sample_duration,
	// which mkvgo's own writer never emits.
	trex := fullBox("trex", 0, 0, func(w *bw) {
		w.u32(1)  // track_ID (the video track above)
		w.u32(1)  // default_sample_description_index
		w.u32(40) // default_sample_duration: 1000 (timescale) / 40 == 25 fps
		w.u32(0)
		w.u32(0)
	})
	fragMoovChildren := bytes.Join([][]byte{mvhd, videoTrak, box("mvex", trex)}, nil)

	mv, err := parseMoov(fragMoovChildren, 0, sampleNone)
	if err != nil {
		t.Fatalf("parseMoov: %v", err)
	}
	if !mv.fragmented {
		t.Fatal("the mvex box must mark the movie fragmented")
	}
	vt := videoTrack(mv)
	if vt == nil {
		t.Fatal("no video track")
	}
	if vt.frameRate != 25 {
		t.Errorf("frameRate = %v, want 25 (1000/40, from the trex default_sample_duration)", vt.frameRate)
	}
}

// --- parse.go: parseMP4Tags meta-without-version/flags fallback (:862) -------

// TestParseMP4TagsMetaWithoutVersionFlags kills the survivor on the "meta atom
// omitted its FullBox version/flags" fallback: every existing ilst fixture
// writes the leading 4 zero bytes, so iterBoxes(meta.payload[4:]) always
// succeeds and the fallback re-parse (iterBoxes(meta.payload) directly) is
// never reached.
func TestParseMP4TagsMetaWithoutVersionFlags(t *testing.T) {
	data := func(s string) []byte {
		return box("data", append([]byte{0, 0, 0, 1, 0, 0, 0, 0}, s...))
	}
	ilst := box("ilst", box("\xa9nam", data("No Version Flags")))
	meta := box("meta", ilst) // no leading version/flags, unlike the normal case
	udtaBoxes, err := iterBoxes(meta)
	if err != nil {
		t.Fatal(err)
	}
	_, title, _, _ := parseMP4Tags(udtaBoxes)
	if title != "No Version Flags" {
		t.Errorf("title = %q, want %q (the meta-without-version/flags fallback must still find the ilst)", title, "No Version Flags")
	}
}

// --- fragparse.go: readFragmentSamples box-header variants (:94,101) ---------

// TestReadFragmentSamplesSizeZeroExtendsToEnd kills the readFragmentSamples
// survivor on the size==0 "to EOF" box: every existing fixture frames its moof
// with an explicit 32-bit size.
func TestReadFragmentSamplesSizeZeroExtendsToEnd(t *testing.T) {
	moofPayload := buildOneSampleMoof(1)[8:] // strip the moof box's own header

	var w bw
	w.u32(0) // size == 0: extends to the end of the parse window
	w.fourcc("moof")
	w.bytes(moofPayload)
	file := w.b

	mv := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := readFragmentSamples(bytes.NewReader(file), int64(len(file)), mv, nil); err != nil {
		t.Fatalf("readFragmentSamples: %v", err)
	}
	if len(mv.tracks[0].samples) != 1 {
		t.Fatalf("samples = %d, want 1 (a size-0 moof must be read to the end of the file)", len(mv.tracks[0].samples))
	}
}

// TestReadFragmentSamplesMoofExceedsLimit kills the readFragmentSamples survivor
// on the maxFragMoofBytes rejection: the check runs before any read of the
// (declared, not actually present) huge payload, so it needs no multi-GB
// allocation to exercise — only a size field large enough to trip it and a
// caller-supplied `size` bound that tolerates it.
func TestReadFragmentSamplesMoofExceedsLimit(t *testing.T) {
	var hdr bw
	hdr.u32(uint32(maxFragMoofBytes + 9)) // header(8) + payload(maxFragMoofBytes+1)
	hdr.fourcc("moof")
	file := hdr.b // only the 8-byte header is physically present

	fakeSize := int64(maxFragMoofBytes) + 100 // large enough that off+boxSize <= size
	mv := &movie{}
	err := readFragmentSamples(bytes.NewReader(file), fakeSize, mv, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("readFragmentSamples: err = %v, want an 'exceeds limit' error", err)
	}
}

// --- fragparse.go: parseTrafSamples base_data_offset read (:185) -------------

// TestParseTrafSamplesBaseDataOffsetRead kills the survivor on the successful
// base_data_offset READ (as opposed to its truncation error, already covered):
// an explicit base_data_offset must be used as the sample's base, overriding
// moofStart entirely.
func TestParseTrafSamplesBaseDataOffsetRead(t *testing.T) {
	tfhd := fullBox("tfhd", 0, tfhdBaseDataOffset|tfhdDefaultDuration|tfhdDefaultSize|tfhdDefaultFlags, func(w *bw) {
		w.u32(1)    // track_ID
		w.u64(9000) // base_data_offset: explicit, must override moofStart
		w.u32(40)   // default_sample_duration
		w.u32(10)   // default_sample_size
		w.u32(0)    // default_sample_flags
	})
	trun := fullBox("trun", 0, 0, func(w *bw) { w.u32(1) })
	traf := append(append([]byte{}, tfhd...), trun...)

	mv := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := parseTrafSamples(traf, 500, nil, mv); err != nil {
		t.Fatalf("parseTrafSamples: %v", err)
	}
	if got := mv.tracks[0].samples[0].offset; got != 9000 {
		t.Errorf("sample offset = %d, want 9000 (explicit base_data_offset must override moofStart 500)", got)
	}
}

// --- fragparse.go: appendTrunSamples first_sample_flags read (:283) ---------

// TestAppendTrunSamplesFirstSampleFlagsOverride kills the survivor on the
// successful first_sample_flags READ (as opposed to its truncation error,
// already covered): when set, it must override sample 0's flags (only), taking
// precedence over the explicit per-sample flags field.
func TestAppendTrunSamplesFirstSampleFlagsOverride(t *testing.T) {
	tr := &inTrack{timescale: 1000, trackID: 1}
	trunFlags := uint32(trunFirstSampleFlag | trunSampleDuration | trunSampleSize | trunSampleFlags)
	full := fullBox("trun", 0, trunFlags, func(w *bw) {
		w.u32(2)             // sample_count
		w.u32(sampleNonSync) // first_sample_flags: mark sample 0 non-sync
		w.u32(40)            // sample0 duration
		w.u32(100)           // sample0 size
		w.u32(0)             // sample0 per-sample flags (would be sync if not overridden)
		w.u32(40)            // sample1 duration
		w.u32(200)           // sample1 size
		w.u32(0)             // sample1 per-sample flags (sync)
	})
	p := full[8:]

	if _, _, err := appendTrunSamples(p, tr, 0, 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("appendTrunSamples: %v", err)
	}
	if len(tr.samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(tr.samples))
	}
	if tr.samples[0].sync {
		t.Error("sample0 must be non-sync: first_sample_flags must override the per-sample flags field")
	}
	if !tr.samples[1].sync {
		t.Error("sample1 must stay sync: first_sample_flags applies only to sample 0")
	}

	// Truncated: the flag is set but the payload is one byte short of the
	// 4-byte first_sample_flags field.
	var short bw
	short.u8(0)
	short.u24(trunFirstSampleFlag)
	short.u32(1) // sample_count
	short.u8(0)
	short.u8(0)
	short.u8(0) // 3 of the 4 first_sample_flags bytes
	tr2 := &inTrack{timescale: 1000, trackID: 1}
	if _, _, err := appendTrunSamples(short.b, tr2, 0, 0, 0, 0, 0, 0); err == nil || !strings.Contains(err.Error(), "trun truncated first_sample_flags") {
		t.Errorf("appendTrunSamples: err = %v, want trun truncated first_sample_flags", err)
	}
}

// --- fragment.go: fillFragTiming last-sample duration branches (:337,339,342) -

// TestFillFragTimingLastDurMsOverride kills the survivor on the `lastDurMs > 0`
// branch: a caller-supplied last-sample duration (audio's frameDurMs) must be
// used verbatim, distinct from both the "same as the previous sample" (n>1) and
// the "single sample" (default) results.
func TestFillFragTimingLastDurMsOverride(t *testing.T) {
	samples := []fragSample{{ptsMs: 0}, {ptsMs: 40}, {ptsMs: 80}}
	_, _, totalTS := fillFragTiming(samples, 777, movieTimescale, 0)
	if got := samples[2].durTS; got != 777 {
		t.Errorf("last sample durTS = %d, want 777 (the caller-supplied lastDurMs, not the previous sample's duration or 1)", got)
	}
	wantTotal := int64(40 + 40 + 777) // two real gaps (40 each) + the overridden last duration
	if totalTS != wantTotal {
		t.Errorf("totalTS = %d, want %d", totalTS, wantTotal)
	}
}

// TestFillFragTimingSingleSampleDefaultDuration kills the survivor on the
// `n > 1` boundary and its `default: durTS = 1` fallback: a fragment holding
// exactly one sample (no lastDurMs hint) can derive nothing from a neighbour,
// so its duration must default to 1 tick, not read samples[-1].
func TestFillFragTimingSingleSampleDefaultDuration(t *testing.T) {
	samples := []fragSample{{ptsMs: 0}}
	_, _, totalTS := fillFragTiming(samples, 0, movieTimescale, 0)
	if got := samples[0].durTS; got != 1 {
		t.Errorf("single-sample durTS = %d, want 1 (the n<=1 default, since there is no lastDurMs and no previous sample)", got)
	}
	if totalTS != 1 {
		t.Errorf("totalTS = %d, want 1", totalTS)
	}
}

// --- mux.go: emitChapterSamples last-chapter EndMs branch (:66,68) -----------

// TestEmitChapterSamplesLastChapterEndMsExact kills the survivor on the `case
// ch.EndMs > ch.StartMs` branch: TestChapterWithEndMs (coverage_test.go) only
// checks that the chapters round-trip through a real MP4, never the exact
// sample duration this branch computes.
func TestEmitChapterSamplesLastChapterEndMsExact(t *testing.T) {
	tr := outTrack{chapterList: []mkv.Chapter{
		{StartMs: 0, Title: "A"},
		{StartMs: 2000, Title: "B", EndMs: 5000}, // last chapter, explicit end
	}}
	var buf bytes.Buffer
	if err := tr.emitChapterSamples(&countWriter{w: &buf}); err != nil {
		t.Fatal(err)
	}
	wantDur := []int64{2000, 3000} // gap(A)=2000-0; last(B)=5000-2000
	for i, s := range tr.samples.samples {
		if s.dur != wantDur[i] {
			t.Errorf("chapter sample %d dur = %d, want %d", i, s.dur, wantDur[i])
		}
	}
}

// TestEmitChapterSamplesLastChapterEndMsEqualStartFallsToDefault kills the `>`
// vs `>=` boundary mutant on `ch.EndMs > ch.StartMs`: an EndMs equal to StartMs
// must NOT take this branch (a zero-length explicit end is not meaningful) and
// must fall through to the defaultCueDurMs default instead.
func TestEmitChapterSamplesLastChapterEndMsEqualStartFallsToDefault(t *testing.T) {
	tr := outTrack{chapterList: []mkv.Chapter{{StartMs: 1000, Title: "Only", EndMs: 1000}}}
	var buf bytes.Buffer
	if err := tr.emitChapterSamples(&countWriter{w: &buf}); err != nil {
		t.Fatal(err)
	}
	if got := tr.samples.samples[0].dur; got != defaultCueDurMs {
		t.Errorf("dur = %d, want %d (EndMs == StartMs must fall to the default, not the EndMs>StartMs branch)", got, defaultCueDurMs)
	}
}

// --- sampletable.go: parseStts n==0 short-circuit (:309) ---------------------

// TestParseSttsNoSamplesExpectedShortCircuits kills the survivor on the `n ==
// 0` branch inside the too-short-payload guard: when the caller expects zero
// samples, a missing/too-short stts must return an empty (not nil-error) table,
// distinct from the n>0 case which must still error.
func TestParseSttsNoSamplesExpectedShortCircuits(t *testing.T) {
	for _, payload := range [][]byte{nil, {0, 0, 0}} {
		durs, err := parseStts(payload, 0)
		if err != nil {
			t.Errorf("parseStts(%v, n=0): err = %v, want nil", payload, err)
		}
		if len(durs) != 0 {
			t.Errorf("parseStts(%v, n=0): durations = %v, want empty", payload, durs)
		}
	}
	if _, err := parseStts(nil, 3); err == nil {
		t.Error("parseStts(nil, n=3): want an error (n>0 still needs a usable stts)")
	}
}

// --- inband_colour.go: firstSampleLoc co64 (64-bit chunk offset) path (:32) --

// TestFirstSampleLocCo64 kills the survivor on the co64 (64-bit chunk offset)
// fallback: no existing fixture omits stco in favour of co64, so this branch —
// the large-file counterpart of stco — was never exercised.
func TestFirstSampleLocCo64(t *testing.T) {
	var stsz bw
	stsz.u32(0)   // version/flags
	stsz.u32(0)   // sample_size == 0: variable, read from entries
	stsz.u32(1)   // entry_count
	stsz.u32(777) // entries[0]

	var co64 bw
	co64.u32(0)           // version/flags
	co64.u32(1)           // entry_count
	co64.u64(0x100000123) // entries[0]: an offset beyond the 32-bit stco range

	stbl := []memBox{{typ: "stsz", payload: stsz.b}, {typ: "co64", payload: co64.b}}
	offset, size := firstSampleLoc(stbl)
	if offset != 0x100000123 || size != 777 {
		t.Errorf("firstSampleLoc(co64) = (%d, %d), want (%d, 777)", offset, size, int64(0x100000123))
	}
}
