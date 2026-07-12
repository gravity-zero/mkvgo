package mp4

import (
	"bytes"
	"testing"
)

// fragparse_mut_test.go kills mutation-testing survivors in fragparse.go: the
// tfhd/tfdt/trun field decoding and the fragment-loop bookkeeping that build a
// fragmented MP4's sample table (offset/size/dts/cts/sync/duration), checked
// against exact expected values.

// TestParseTrexDefaultsLengthBoundary kills the CONDITIONALS_NEGATION on
// `len(b.payload) >= 24` (fragparse.go:57): a trex payload of exactly 24 bytes
// (every fixed field present) must be read; one byte short must be rejected.
func TestParseTrexDefaultsLengthBoundary(t *testing.T) {
	full := bytesU32(0, 2, 1, 1500, 200, 0x10000) // vf, trackID, sampleDescIdx, dur, size, flags
	moovBoxes := []memBox{{typ: "mvex", payload: box("trex", full)}}
	got := parseTrexDefaults(moovBoxes)
	d, ok := got[2]
	if !ok || d.duration != 1500 || d.size != 200 || d.flags != 0x10000 {
		t.Errorf("trex(24 bytes) = %+v ok=%v, want {1500 200 65536} true", d, ok)
	}

	short := full[:len(full)-1]
	moovBoxes2 := []memBox{{typ: "mvex", payload: box("trex", short)}}
	if _, ok := parseTrexDefaults(moovBoxes2)[2]; ok {
		t.Error("a 23-byte trex must be rejected (too short for the fixed fields)")
	}
}

// buildOneSampleMoof builds a minimal, valid fragment (moof > traf > tfhd +
// tfdt + trun) declaring one sample entirely from tfhd defaults: duration 40
// (media ticks), size 10, sync (flags 0), at decode time 0.
func buildOneSampleMoof(trackID uint32) []byte {
	tfhd := fullBox("tfhd", 0, tfhdDefaultDuration|tfhdDefaultSize|tfhdDefaultFlags, func(w *bw) {
		w.u32(trackID)
		w.u32(40) // default_sample_duration
		w.u32(10) // default_sample_size
		w.u32(0)  // default_sample_flags (sync)
	})
	tfdt := fullBox("tfdt", 0, 0, func(w *bw) { w.u32(0) })
	trun := fullBox("trun", 0, 0, func(w *bw) { w.u32(1) }) // 1 sample, all defaults
	traf := container("traf", tfhd, tfdt, trun)
	return container("moof", traf)
}

// TestReadFragmentSamplesTailBoundary kills the CONDITIONALS_BOUNDARY on
// `off+boxSize > size` (fragparse.go:96) and the CONDITIONALS_NEGATION /
// ARITHMETIC_BASE on the post-loop `n > 0` guard and `last.ctsMs+last.durMs`
// (fragparse.go:117/120): a fragment that exactly fits the declared file size
// must be read (producing its sample and updating frameCount/duration); one
// byte short must be silently dropped (per the documented truncated-tail
// tolerance), not read out of bounds.
func TestReadFragmentSamplesTailBoundary(t *testing.T) {
	moof := buildOneSampleMoof(1)

	mv := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := readFragmentSamples(bytes.NewReader(moof), int64(len(moof)), mv, nil); err != nil {
		t.Fatalf("exact-size file: %v", err)
	}
	tr := &mv.tracks[0]
	if len(tr.samples) != 1 {
		t.Fatalf("exact-size file: samples = %d, want 1 (a fragment that fits exactly must be read)", len(tr.samples))
	}
	if tr.frameCount != 1 {
		t.Errorf("frameCount = %d, want 1", tr.frameCount)
	}
	if mv.durationMs != 40 {
		t.Errorf("mv.durationMs = %d, want 40 (last sample's cts 0 + dur 40)", mv.durationMs)
	}

	mv2 := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := readFragmentSamples(bytes.NewReader(moof), int64(len(moof))-1, mv2, nil); err != nil {
		t.Fatalf("truncated-by-1 file: %v", err)
	}
	if len(mv2.tracks[0].samples) != 0 {
		t.Errorf("truncated-by-1 file: samples = %d, want 0 (a fragment that doesn't fit must be dropped)", len(mv2.tracks[0].samples))
	}
	if mv2.durationMs != 0 {
		t.Errorf("truncated-by-1 file: mv.durationMs = %d, want 0 (never touched)", mv2.durationMs)
	}
}

// TestParseMoofSamplesPropagatesTrafError kills the CONDITIONALS_NEGATION on
// `err != nil` after parseTrafSamples (fragparse.go:137): a malformed traf
// (a tfhd declaring base_data_offset but too short to carry it) must fail the
// whole moof, not be silently ignored.
func TestParseMoofSamplesPropagatesTrafError(t *testing.T) {
	badTfhd := fullBox("tfhd", 0, tfhdBaseDataOffset, func(w *bw) {
		w.u32(1) // track_ID only; the flag demands 8 more bytes that are missing
	})
	traf := container("traf", badTfhd)
	moof := container("moof", traf)

	mv := &movie{}
	if err := parseMoofSamples(moof[8:], 0, nil, mv); err == nil {
		t.Error("a traf error must fail the moof parse, not be swallowed")
	}
}

// TestParseTrafSamplesDefaultsFromTfhd kills the CONDITIONALS_NEGATION on the
// tfhd default_sample_duration/default_sample_size flag checks (fragparse.go:
// 192, 195): when the flags are set, the defaults they carry must be used  -
// not the (here, zero) trex fallback.
func TestParseTrafSamplesDefaultsFromTfhd(t *testing.T) {
	tfhd := fullBox("tfhd", 0, tfhdDefaultDuration|tfhdDefaultSize|tfhdDefaultFlags, func(w *bw) {
		w.u32(1)    // track_ID
		w.u32(1000) // default_sample_duration
		w.u32(50)   // default_sample_size
		w.u32(0)    // default_sample_flags (sync)
	})
	tfdt := fullBox("tfdt", 0, 0, func(w *bw) { w.u32(200) })
	trun := fullBox("trun", 0, 0, func(w *bw) { w.u32(1) })
	traf := append(append(append([]byte{}, tfhd...), tfdt...), trun...)

	mv := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := parseTrafSamples(traf, 500, map[uint32]trexDefault{}, mv); err != nil {
		t.Fatalf("parseTrafSamples: %v", err)
	}
	if len(mv.tracks[0].samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(mv.tracks[0].samples))
	}
	s := mv.tracks[0].samples[0]
	if s.size != 50 {
		t.Errorf("size = %d, want 50 (from tfhd default_sample_size)", s.size)
	}
	if s.durMs != 1000 {
		t.Errorf("durMs = %d, want 1000 (from tfhd default_sample_duration)", s.durMs)
	}
	if s.dtsMs != 200 {
		t.Errorf("dtsMs = %d, want 200 (tfdt baseDecodeTime)", s.dtsMs)
	}
	if !s.sync {
		t.Error("sync should be true (default_sample_flags has no non-sync bit)")
	}
	if s.offset != 500 {
		t.Errorf("offset = %d, want 500 (moofStart, the default base)", s.offset)
	}
}

// TestParseTrafSamplesTfdtLengthBoundary kills the CONDITIONALS_BOUNDARY on
// `len(b.payload) >= 8` (fragparse.go:213): a version-0 tfdt payload of
// exactly 8 bytes must be read; 7 bytes must be silently ignored (decode time
// stays at its zero default), not crash.
func TestParseTrafSamplesTfdtLengthBoundary(t *testing.T) {
	tfhd := fullBox("tfhd", 0, 0, func(w *bw) { w.u32(1) })
	trun := fullBox("trun", 0, 0, func(w *bw) { w.u32(1) })

	validTfdt := box("tfdt", []byte{0, 0, 0, 0, 0, 0, 3, 9}) // vf(4) + baseDecodeTime=0x0309=777
	traf := append(append(append([]byte{}, tfhd...), validTfdt...), trun...)
	mv := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := parseTrafSamples(traf, 0, nil, mv); err != nil {
		t.Fatalf("parseTrafSamples: %v", err)
	}
	if got := mv.tracks[0].samples[0].dtsMs; got != 777 {
		t.Errorf("dtsMs = %d, want 777 (exactly-8-byte tfdt payload must be read)", got)
	}

	shortTfdt := box("tfdt", []byte{0, 0, 0, 0, 0, 0, 3}) // 7-byte payload
	traf2 := append(append(append([]byte{}, tfhd...), shortTfdt...), trun...)
	mv2 := &movie{tracks: []inTrack{{trackID: 1, timescale: 1000}}}
	if err := parseTrafSamples(traf2, 0, nil, mv2); err != nil {
		t.Fatalf("parseTrafSamples: %v", err)
	}
	if got := mv2.tracks[0].samples[0].dtsMs; got != 0 {
		t.Errorf("dtsMs = %d, want 0 (a too-short tfdt must be ignored)", got)
	}
}

// TestAppendTrunSamplesFieldsAndSync kills the CONDITIONALS_NEGATION on
// `sflags&sampleNonSync == 0` (fragparse.go:339) by checking both sync states
// from explicit per-sample flags, and exercises the per-sample offset/dts/cts
// bookkeeping along the way.
func TestAppendTrunSamplesFieldsAndSync(t *testing.T) {
	tr := &inTrack{timescale: 1000, trackID: 1}
	trunFlags := uint32(trunSampleDuration | trunSampleSize | trunSampleFlags | trunSampleCTS)
	full := fullBox("trun", 1, trunFlags, func(w *bw) {
		w.u32(2)
		w.u32(40)
		w.u32(100)
		w.u32(sampleNonSync)
		w.i32(5)
		w.u32(40)
		w.u32(200)
		w.u32(0)
		w.i32(-3)
	})
	p := full[8:] // the trun box's payload, as iterBoxes would hand it

	sampleOff, newDTS, err := appendTrunSamples(p, tr, 1000, 1000, 0, 999, 999, 999)
	if err != nil {
		t.Fatalf("appendTrunSamples: %v", err)
	}
	if len(tr.samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(tr.samples))
	}
	s0, s1 := tr.samples[0], tr.samples[1]
	if s0.offset != 1000 || s0.size != 100 || s0.dtsMs != 0 || s0.ctsMs != 5 || s0.sync {
		t.Errorf("sample0 = %+v, want offset 1000 size 100 dts 0 cts 5 sync=false", s0)
	}
	if s1.offset != 1100 || s1.size != 200 || s1.dtsMs != 40 || s1.ctsMs != 37 || !s1.sync {
		t.Errorf("sample1 = %+v, want offset 1100 size 200 dts 40 cts 37 sync=true", s1)
	}
	if sampleOff != 1300 {
		t.Errorf("sampleOff = %d, want 1300", sampleOff)
	}
	if newDTS != 80 {
		t.Errorf("newDTS = %d, want 80", newDTS)
	}
}
