package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// parse_mut_test.go kills mutation-testing survivors in parse.go: the box
// header/size arithmetic, the lazy-moov reader's bookkeeping, the fragmented
// random-access index readers, and the per-track metadata derivations —
// checked against exact parsed values or the accept/reject decision on
// deliberately malformed input.

// --- iterBoxes ---------------------------------------------------------------

// TestIterBoxes64BitBoxExactBoundary kills the ARITHMETIC_BASE on `off+16`
// (parse.go:132), with the box positioned at a nonzero offset so a mutated
// arithmetic op (off-16, off*16, ...) cannot coincidentally agree with the
// real check at off==0.
func TestIterBoxes64BitBoxExactBoundary(t *testing.T) {
	first := box("free", nil) // 8 bytes; the 64-bit box under test starts at off=8

	var w bw
	w.u32(1) // size==1 signals the 64-bit largesize form
	w.fourcc("free")
	w.u64(24) // largesize = 16-byte header + 8-byte payload
	w.zeros(8)
	second := w.b // 24 bytes total

	buf := append(append([]byte{}, first...), second...)

	if boxes, err := iterBoxes(buf); err != nil || len(boxes) != 2 {
		t.Fatalf("well-formed 64-bit box: boxes=%d err=%v, want 2/nil", len(boxes), err)
	}

	// off+16 == len(buf): exactly the 16-byte largesize header, no payload
	// bytes needed for the truncation check itself — must NOT be rejected as
	// truncated (a later, unrelated check may still reject the box for
	// lacking its declared payload, but not with this specific error).
	exact16 := buf[:len(first)+16]
	if _, err := iterBoxes(exact16); err != nil && strings.Contains(err.Error(), "truncated 64-bit box") {
		t.Errorf("off+16 == len(buf) must not be reported as a truncated 64-bit header, got %v", err)
	}

	// One byte short of the 16-byte header: must be rejected as truncated.
	truncated := buf[:len(first)+15]
	if _, err := iterBoxes(truncated); err == nil || !strings.Contains(err.Error(), "truncated 64-bit box") {
		t.Errorf("off+16 == len(buf)+1 must be rejected as truncated, got %v", err)
	}
}

// TestIterBoxesSizeZeroFillsExactRemainder kills the INVERT_NEGATIVES and
// ARITHMETIC_BASE mutants on `len(buf) - off` (parse.go:138), with the box at
// a nonzero offset so the arithmetic actually matters.
func TestIterBoxesSizeZeroFillsExactRemainder(t *testing.T) {
	first := box("free", nil) // 8 bytes
	tail := []byte("REMAINDER-BYTES-0123456789")

	var w bw
	w.u32(0) // size==0: fills to the end of the buffer
	w.fourcc("skip")
	w.bytes(tail)
	second := w.b

	buf := append(append([]byte{}, first...), second...)
	boxes, err := iterBoxes(buf)
	if err != nil {
		t.Fatalf("iterBoxes: %v", err)
	}
	if len(boxes) != 2 {
		t.Fatalf("boxes = %d, want 2", len(boxes))
	}
	if !bytes.Equal(boxes[1].payload, tail) {
		t.Errorf("size-0 box payload = %q, want %q (len(buf)-off exactly)", boxes[1].payload, tail)
	}
}

// --- validMoovAt ---------------------------------------------------------------

// TestValidMoovAtBoundaries kills the CONDITIONALS_BOUNDARY on `boxStart+16 >
// size` and the CONDITIONALS_BOUNDARY/INVERT_NEGATIVES on `childSize < 8 ||
// childSize > boxSize-8` (parse.go:257, 268). The reader always physically
// has the bytes; only the `size` parameter (the trusted file length) varies,
// isolating the check from mere read availability.
func TestValidMoovAtBoundaries(t *testing.T) {
	mkBuf := func(childSize uint32) []byte {
		b := make([]byte, 16)
		binary.BigEndian.PutUint32(b[8:12], childSize)
		copy(b[12:16], "mvhd")
		return b
	}

	buf := mkBuf(8)
	if !validMoovAt(bytes.NewReader(buf), 0, 24, 16) {
		t.Error("boxStart+16 == size must be accepted")
	}
	if validMoovAt(bytes.NewReader(buf), 0, 24, 15) {
		t.Error("boxStart+16 == size+1 must be rejected, even though the reader has the bytes")
	}

	if !validMoovAt(bytes.NewReader(mkBuf(8)), 0, 24, 16) {
		t.Error("childSize == 8 must be accepted")
	}
	if validMoovAt(bytes.NewReader(mkBuf(7)), 0, 24, 16) {
		t.Error("childSize == 7 must be rejected (< 8)")
	}
	if !validMoovAt(bytes.NewReader(mkBuf(16)), 0, 24, 16) { // boxSize-8 == 24-8 == 16
		t.Error("childSize == boxSize-8 must be accepted")
	}
	if validMoovAt(bytes.NewReader(mkBuf(17)), 0, 24, 16) {
		t.Error("childSize == boxSize-8+1 must be rejected")
	}
}

// --- lazyMoov ------------------------------------------------------------------

// errAfterNReader returns n zero bytes then a permanent read error, for
// exercising error-path propagation without a real truncated file.
type errAfterNReader struct {
	n, pos int
}

func (e *errAfterNReader) Read(p []byte) (int, error) {
	if e.pos >= e.n {
		return 0, io.ErrClosedPipe
	}
	k := e.n - e.pos
	if k > len(p) {
		k = len(p)
	}
	e.pos += k
	return k, nil
}
func (e *errAfterNReader) Seek(offset int64, whence int) (int64, error) { return offset, nil }

// TestLazyMoovEnsurePropagatesReadError kills the CONDITIONALS_NEGATION on
// `err != nil` after io.ReadFull (parse.go:370): a real read error must
// propagate, not be swallowed with filled advanced as if it succeeded.
func TestLazyMoovEnsurePropagatesReadError(t *testing.T) {
	m := &lazyMoov{r: &errAfterNReader{n: 4}, buf: make([]byte, 20)}
	if err := m.ensure(20); err == nil {
		t.Error("a read error partway through must propagate")
	}
}

// TestLazyMoovParseLoopBoundary kills the CONDITIONALS_BOUNDARY on `off+8 <=
// end` (parse.go:386): a single 8-byte box that exactly fills the parse
// window must still be read, not skipped.
func TestLazyMoovParseLoopBoundary(t *testing.T) {
	raw := box("free", nil) // 8 bytes, no payload
	m := &lazyMoov{r: bytes.NewReader(raw), buf: make([]byte, len(raw))}
	if err := m.parse(0, len(raw)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.filled != len(raw) {
		t.Errorf("filled = %d, want %d (the exactly-fitting final box must be read)", m.filled, len(raw))
	}
	if string(m.buf[4:8]) != "free" {
		t.Errorf("buf = %q, want the real disk bytes read into it", m.buf)
	}
}

// TestLazyMoovParseSizeCheckBoundary kills the ARITHMETIC_BASE on
// `off+int(boxSize)` (parse.go:406): a box whose declared size exactly
// reaches the end of the parse window is valid; one byte more is not.
func TestLazyMoovParseSizeCheckBoundary(t *testing.T) {
	raw := box("free", make([]byte, 8)) // 16-byte box (8 header + 8 payload)
	m := &lazyMoov{r: bytes.NewReader(raw), buf: make([]byte, len(raw))}
	if err := m.parse(0, len(raw)); err != nil {
		t.Fatalf("off+boxSize == end must be accepted: %v", err)
	}
	m2 := &lazyMoov{r: bytes.NewReader(raw), buf: make([]byte, len(raw))}
	if err := m2.parse(0, len(raw)-1); err == nil {
		t.Error("off+boxSize == end+1 must be rejected as an invalid size")
	}
}

// Note on parse.go:417-418 (the lazySkipBody "keep" threshold arithmetic,
// off+hdr+lazyKeepHead): not tested here. ensure() always coalesces reads up
// to at least lazyChunk (64KiB) ahead of the current position (capped only by
// the buffer's total length), and the per-box header read that precedes every
// switch case already triggers that coalescing. Since hdr+lazyKeepHead (24)
// is far smaller than lazyChunk, ensure(keep) is a no-op in every reachable
// layout: keep's specific value never changes what has already been fetched.
// Verified empirically: forcing keep negative (so the box's own head is never
// explicitly requested) still leaves TestLazyMoovMetaParity green, because
// the preceding coalesced read already covers it. Equivalent in practice.

// --- fragmented random-access index readers ------------------------------------

// TestReadFragmentKeyframesMfraSizeExactlyWholeFile kills the
// CONDITIONALS_BOUNDARY on `mfraSize > size` (parse.go:515): a file that IS
// exactly its own mfra (no prefix at all) must be accepted, not rejected.
func TestReadFragmentKeyframesMfraSizeExactlyWholeFile(t *testing.T) {
	tfra := box("tfra", tfraPayload(1, []uint32{0, 500}))
	mfraSize := 8 + len(tfra) + 16 // mfra header + tfra + mfro(16)
	mfro := box("mfro", append([]byte{0, 0, 0, 0}, u32be(uint32(mfraSize))...))
	mfra := box("mfra", append(append([]byte{}, tfra...), mfro...))
	if len(mfra) != mfraSize {
		t.Fatalf("mfra size %d != computed %d", len(mfra), mfraSize)
	}

	mv := &movie{tracks: []inTrack{{trackID: 1, trackType: mkv.VideoTrack, timescale: 1000}}}
	readFragmentKeyframes(bytes.NewReader(mfra), int64(len(mfra)), mv)
	if want := []int64{0, 500}; !reflect.DeepEqual(mv.tracks[0].keyframesMs, want) {
		t.Errorf("keyframes = %v, want %v (mfraSize == size must be accepted)", mv.tracks[0].keyframesMs, want)
	}
}

// TestReadSidxKeyframesEditShiftArithmetic kills the ARITHMETIC_BASE on
// `t + vt.editShiftMs` (parse.go:621).
func TestReadSidxKeyframesEditShiftArithmetic(t *testing.T) {
	sidx := box("sidx", sidxPayload(1, 1000, 0, []sidxEntry{
		{dur: 2000, sap: true, sapType: 1},
		{dur: 2000, sap: true, sapType: 1},
	}))
	file := bytes.Join([][]byte{sidx, box("moof", make([]byte, 8))}, nil)
	mv := &movie{tracks: []inTrack{{trackID: 1, trackType: mkv.VideoTrack, timescale: 1000, editShiftMs: 300}}}
	readSidxKeyframes(bytes.NewReader(file), int64(len(file)), mv)
	want := []int64{300, 2300} // (0+300), (2000+300)
	if !reflect.DeepEqual(mv.tracks[0].keyframesMs, want) {
		t.Errorf("keyframes = %v, want %v (t + editShiftMs)", mv.tracks[0].keyframesMs, want)
	}
}

// TestScanSidxBoxesTailBoundary kills the CONDITIONALS_BOUNDARY on `off+8 <=
// size` (parse.go:635), the CONDITIONALS_BOUNDARY/ARITHMETIC_BASE on
// `boxSize < headerLen || off+boxSize > size` (parse.go:655), and the
// ARITHMETIC_BASE on `boxSize-headerLen` (parse.go:660): a leading box that
// exactly fills its own space must be consumed (not left unscanned) so the
// scan reaches a following sidx, whose payload length/content must be exact.
func TestScanSidxBoxesTailBoundary(t *testing.T) {
	tiny := box("free", nil) // 8 bytes, off+8 == size at off=0
	sidxPayloadBytes := sidxPayload(1, 1000, 0, []sidxEntry{{dur: 1000, sap: true, sapType: 1}})
	sidx := box("sidx", sidxPayloadBytes)
	file := append(append([]byte{}, tiny...), sidx...)

	got := scanSidxBoxes(bytes.NewReader(file), int64(len(file)))
	if len(got) != 1 {
		t.Fatalf("sidx boxes = %d, want 1 (the exact-fit leading box must be consumed)", len(got))
	}
	if !bytes.Equal(got[0], sidxPayloadBytes) {
		t.Errorf("sidx payload = % x, want % x (boxSize-headerLen exactly)", got[0], sidxPayloadBytes)
	}
}

// --- parseMoov / parseTrak -----------------------------------------------------

// mvhdV1 builds a version-1 mvhd (64-bit duration) with the given timescale
// and duration ticks.
func mvhdV1(timescale uint32, durTicks uint64) []byte {
	return fullBox("mvhd", 1, 0, func(w *bw) {
		w.u64(0) // creation_time
		w.u64(0) // modification_time
		w.u32(timescale)
		w.u64(durTicks)
		w.u32(0x00010000)
		w.u16(0x0100)
		w.u16(0)
		w.zeros(8)
		w.matrix(unityMatrix)
		w.zeros(24)
		w.u32(2) // next_track_ID
	})
}

// TestParseMoovDurationTicksBoundary kills the CONDITIONALS_BOUNDARY on
// `durTicks < 1<<62` (parse.go:756): an ordinary duration must be accepted
// and scaled; a duration at the sentinel-overflow boundary (1<<62 exactly)
// must be rejected (mv.durationMs stays 0), not used.
func TestParseMoovDurationTicksBoundary(t *testing.T) {
	trak := container("trak", craftTrak("text", "tx3g", "", 1))

	ok := append(mvhdV1(1000, 5000), trak...)
	mv, err := parseMoov(ok, 1<<20, sampleNone)
	if err != nil {
		t.Fatalf("parseMoov: %v", err)
	}
	if mv.durationMs != 5000 {
		t.Errorf("durationMs = %d, want 5000", mv.durationMs)
	}

	rejected := append(mvhdV1(1000, 1<<62), trak...)
	mv2, err := parseMoov(rejected, 1<<20, sampleNone)
	if err != nil {
		t.Fatalf("parseMoov: %v", err)
	}
	if mv2.durationMs != 0 {
		t.Errorf("durationMs = %d, want 0 (durTicks == 1<<62 must be rejected as a sentinel/overflow)", mv2.durationMs)
	}
}

// craftHdlrPayload builds an hdlr box payload with the given media handler and
// a name of nameBytes 'X' characters after the fixed 24-byte header, for
// exercising the hdlr-name fallback boundary exactly.
func craftHdlrPayload(handlerType string, nameBytes int) []byte {
	p := make([]byte, 24)
	copy(p[8:12], handlerType)
	p = append(p, bytes.Repeat([]byte("X"), nameBytes)...)
	return p
}

// craftMinimalSubtitleTrak builds a trak (tkhd+mdia, unwrapped) for a tx3g
// subtitle track with a controllable hdlr name and optional existing
// udta/name box, for the hdlr-name-fallback tests.
func craftMinimalSubtitleTrak(trackID uint32, hdlrNameBytes int, existingName string) []byte {
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(1000)
		w.u32(0)
		w.u16(0)
		w.u16(0)
	})
	hdlr := box("hdlr", craftHdlrPayload("text", hdlrNameBytes))
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(box("tx3g", make([]byte, 8)))
	})
	mdia := container("mdia", mdhd, hdlr, container("minf", container("stbl", stsd)))
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(trackID)
		w.u32(0)
		w.u32(0)
	})
	out := append(append([]byte{}, tkhd...), mdia...)
	if existingName != "" {
		out = append(out, container("udta", boxf("name", func(w *bw) { w.bytes([]byte(existingName)) }))...)
	}
	return out
}

// TestParseTrakHdlrNameFallback kills the two CONDITIONALS_NEGATION mutants on
// `tr.name == "" && len(hdlr.payload) > 24` (parse.go:1116): the fallback
// name is used only when there is no existing name AND the hdlr actually
// carries name bytes beyond its fixed 24-byte header.
func TestParseTrakHdlrNameFallback(t *testing.T) {
	// Exactly one name byte (len(hdlr.payload) == 25): must be adopted.
	tr, _, err := parseTrak(craftMinimalSubtitleTrak(1, 1, ""), 1<<20, 1000, sampleNone)
	if err != nil {
		t.Fatalf("parseTrak: %v", err)
	}
	if tr.name != "X" {
		t.Errorf("name = %q, want %q (hdlr fallback with 1 name byte)", tr.name, "X")
	}

	// Exactly the 24-byte fixed header, no name bytes: must stay empty.
	tr2, _, err := parseTrak(craftMinimalSubtitleTrak(2, 0, ""), 1<<20, 1000, sampleNone)
	if err != nil {
		t.Fatalf("parseTrak: %v", err)
	}
	if tr2.name != "" {
		t.Errorf("name = %q, want empty (hdlr.payload len == 24, no room for a name)", tr2.name)
	}

	// An existing udta/name must NOT be overridden by the hdlr fallback.
	tr3, _, err := parseTrak(craftMinimalSubtitleTrak(3, 5, "FromUdta"), 1<<20, 1000, sampleNone)
	if err != nil {
		t.Fatalf("parseTrak: %v", err)
	}
	if tr3.name != "FromUdta" {
		t.Errorf("name = %q, want %q (an existing name must not be overridden)", tr3.name, "FromUdta")
	}
}

// TestParseTrakEditShiftArithmetic kills the INVERT_NEGATIVES/ARITHMETIC_BASE
// mutants on `emptyMs - ticksToMs(editMediaTime, tr.timescale)` (parse.go:1143)
// for a non-audio track (video/subtitle keep the net shift, unlike audio,
// which carries the trim as CodecDelay instead).
func TestParseTrakEditShiftArithmetic(t *testing.T) {
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(1000) // timescale
		w.u32(0)
		w.u16(0)
		w.u16(0)
	})
	hdlr := box("hdlr", craftHdlrPayload("text", 0))
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(box("tx3g", make([]byte, 8)))
	})
	mdia := container("mdia", mdhd, hdlr, container("minf", container("stbl", stsd)))
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(1)
		w.u32(0)
		w.u32(0)
	})
	elst := fullBox("elst", 0, 0, func(w *bw) {
		w.u32(2)
		w.u32(200)
		w.i32(-1) // empty edit: 200 movie ticks (== ms at movieTS 1000)
		w.u16(1)
		w.u16(0)
		w.u32(999)
		w.i32(70) // non-empty edit: media_time 70 (media ticks == ms here)
		w.u16(1)
		w.u16(0)
	})
	edts := container("edts", elst)
	trak := bytes.Join([][]byte{tkhd, edts, mdia}, nil)

	tr, dropped, err := parseTrak(trak, 1<<20, 1000, sampleNone)
	if err != nil {
		t.Fatalf("parseTrak: %v", err)
	}
	if dropped != nil {
		t.Fatalf("track unexpectedly dropped: %+v", dropped)
	}
	// emptyMs (200) - ticksToMs(70, 1000) (70) = 130.
	if tr.editShiftMs != 130 {
		t.Errorf("editShiftMs = %d, want 130 (200 - 70)", tr.editShiftMs)
	}
}

// TestParseTrakSampleFullPropagatesBuildSampleTableError kills the
// CONDITIONALS_NEGATION on `err != nil` around buildSampleTable
// (parse.go:1210): a malformed stbl (missing stsz) must fail sampleFull
// parsing, not be silently swallowed.
func TestParseTrakSampleFullPropagatesBuildSampleTableError(t *testing.T) {
	_, _, err := parseTrak(craftTrak("text", "tx3g", "", 9), 1<<20, 1000, sampleFull)
	if err == nil {
		t.Error("a stbl without stsz must fail sampleFull parsing")
	}
}

// --- headerConstantFrameDurNs ---------------------------------------------------

// TestHeaderConstantFrameDurNsBoundaryAndArithmetic kills the CONDITIONALS_BOUNDARY
// on `len(stts.payload) < 16` (parse.go:1245), the CONDITIONALS_NEGATION on
// `timescale == 0` (parse.go:1241, whose inversion would divide by zero), the
// CONDITIONALS_NEGATION on `entry_count != 1` (parse.go:1248), and the
// ARITHMETIC_BASE round-to-nearest formula (parse.go:1255).
func TestHeaderConstantFrameDurNsBoundaryAndArithmetic(t *testing.T) {
	stts16 := bytesU32(0, 1, 999, 3) // vf, entry_count=1, sample_count=999, delta=3
	boxes := []memBox{{typ: "stts", payload: stts16}}

	if got := headerConstantFrameDurNs(boxes, 4); got != 750_000_000 {
		t.Errorf("headerConstantFrameDurNs(delta 3, ts 4) = %d, want 750000000", got)
	}
	short := []memBox{{typ: "stts", payload: stts16[:15]}}
	if got := headerConstantFrameDurNs(short, 4); got != 0 {
		t.Errorf("15-byte stts = %d, want 0 (rejected)", got)
	}
	if got := headerConstantFrameDurNs(boxes, 0); got != 0 {
		t.Errorf("timescale 0 = %d, want 0 (must return early, not divide by zero)", got)
	}
	varRate := bytesU32(0, 2, 1, 40, 1, 41) // entry_count = 2: not constant-rate
	if got := headerConstantFrameDurNs([]memBox{{typ: "stts", payload: varRate}}, 1000); got != 0 {
		t.Errorf("entry_count=2 = %d, want 0", got)
	}
}

// --- parseKind -------------------------------------------------------------------

// TestParseKindEmptyScheme kills the CONDITIONALS_BOUNDARY on `i < 0`
// (parse.go's parseKind): a payload whose scheme is the empty string (a null
// byte immediately after the version/flags) must yield scheme="" and the
// value that follows — not treat the whole remainder as the scheme.
func TestParseKindEmptyScheme(t *testing.T) {
	payload := append([]byte{0, 0, 0, 0}, 0) // vf(4) + an immediate null (empty scheme)
	payload = append(payload, []byte("myvalue")...)
	payload = append(payload, 0)
	scheme, value := parseKind(payload)
	if scheme != "" || value != "myvalue" {
		t.Errorf("parseKind = (%q, %q), want (\"\", \"myvalue\")", scheme, value)
	}
}

// --- parseSampleEntry / stsd -----------------------------------------------------

// TestParseSampleEntryStsdLengthBoundary kills the CONDITIONALS_BOUNDARY on
// `len(stsdPayload) < 8` (parse.go:1519) by checking the exact rejection
// message at the boundary: exactly 8 bytes (no entries) must reach the
// "no sample entry" error, not the "too short" one a byte earlier.
func TestParseSampleEntryStsdLengthBoundary(t *testing.T) {
	var tr inTrack
	_, _, err := parseSampleEntry(&tr, bytesU32(0, 0)) // vf, entry_count=0: exactly 8 bytes
	if err == nil || !strings.Contains(err.Error(), "no sample entry") {
		t.Errorf("8-byte stsd err = %v, want \"no sample entry\"", err)
	}
	_, _, err = parseSampleEntry(&tr, bytesU32(0, 0)[:7])
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("7-byte stsd err = %v, want \"too short\"", err)
	}
}

// wrapStsd builds a full stsd payload (fullbox header + entry_count=1 +
// entry) for feeding directly to parseSampleEntry.
func wrapStsd(entry []byte) []byte {
	return append(bytesU32(0, 1), entry...)
}

// TestAC3ChannelOverwriteBoundary kills the CONDITIONALS_BOUNDARY on
// `ac3Channels(dac3) > 0` (parse.go:1564): when the dac3 box can't yield a
// channel count (truncated), the AudioSampleEntry's own channel count must be
// preserved, not zeroed.
func TestAC3ChannelOverwriteBoundary(t *testing.T) {
	audioFixed := make([]byte, 28)
	binary.BigEndian.PutUint16(audioFixed[16:18], 2) // baseline channels = 2
	dac3 := box("dac3", []byte{0})                   // far too short to yield a channel count
	entry := box("ac-3", append(audioFixed, dac3...))

	var tr inTrack
	ok, _, err := parseSampleEntry(&tr, wrapStsd(entry))
	if err != nil || !ok {
		t.Fatalf("parseSampleEntry(ac-3): ok=%v err=%v", ok, err)
	}
	if tr.channels != 2 {
		t.Errorf("channels = %d, want 2 (preserved when dac3 yields 0)", tr.channels)
	}
}

// TestEAC3ChannelOverwrite kills the CONDITIONALS_NEGATION on `err == nil`
// after childConfig (parse.go:1572) and the CONDITIONALS_BOUNDARY on
// `eac3Channels(dec3) > 0` (parse.go:1573): a real dec3 must overwrite the
// base channel count (proving the success branch runs), while a truncated one
// must preserve it (proving the >0 guard, not an unconditional overwrite).
func TestEAC3ChannelOverwrite(t *testing.T) {
	audioFixed := func(ch uint16) []byte {
		p := make([]byte, 28)
		binary.BigEndian.PutUint16(p[16:18], ch)
		return p
	}

	shortDec3 := box("dec3", []byte{0})
	entry := box("ec-3", append(audioFixed(3), shortDec3...))
	var tr inTrack
	ok, _, err := parseSampleEntry(&tr, wrapStsd(entry))
	if err != nil || !ok {
		t.Fatalf("short dec3: ok=%v err=%v", ok, err)
	}
	if tr.channels != 3 {
		t.Errorf("channels = %d, want 3 (preserved when dec3 yields 0)", tr.channels)
	}

	// acmod=3 (3 channels) + lfeon=1, numDepSub=0 -> 4 channels.
	var w bitWriter
	w.write(0, 13) // data_rate
	w.write(0, 3)  // num_ind_sub - 1
	w.write(0, 2)  // fscod
	w.write(8, 5)  // bsid
	w.write(0, 1)  // reserved
	w.write(0, 1)  // asvc
	w.write(0, 3)  // bsmod
	w.write(3, 3)  // acmod
	w.write(1, 1)  // lfeon
	w.write(0, 3)  // reserved
	w.write(0, 4)  // num_dep_sub = 0
	w.write(0, 1)  // reserved
	dec3 := box("dec3", w.bytes())
	entry2 := box("ec-3", append(audioFixed(2), dec3...))
	var tr2 inTrack
	ok2, _, err2 := parseSampleEntry(&tr2, wrapStsd(entry2))
	if err2 != nil || !ok2 {
		t.Fatalf("real dec3: ok=%v err=%v", ok2, err2)
	}
	if tr2.channels != 4 {
		t.Errorf("channels = %d, want 4 (from a real dec3: proves childConfig success is honoured)", tr2.channels)
	}
}

// TestAACChannelOverwriteBoundary kills the CONDITIONALS_BOUNDARY on
// `cfg.channels > 0` (parse.go:1618): a channelConfiguration of 0 (carried in
// a program_config_element mkvgo doesn't resolve) must preserve the base
// channel count, not zero it.
func TestAACChannelOverwriteBoundary(t *testing.T) {
	var w bitWriter
	w.write(2, 5) // audioObjectType = 2 (AAC LC)
	w.write(4, 4) // samplingFrequencyIndex
	w.write(0, 4) // channelConfiguration = 0
	asc := w.bytes()

	audioFixed := make([]byte, 28)
	binary.BigEndian.PutUint16(audioFixed[16:18], 5) // baseline channels = 5
	entry := box("mp4a", append(audioFixed, esdsBox(0x40, asc)...))

	var tr inTrack
	ok, _, err := parseSampleEntry(&tr, wrapStsd(entry))
	if err != nil || !ok {
		t.Fatalf("parseSampleEntry(mp4a): ok=%v err=%v", ok, err)
	}
	if tr.channels != 5 {
		t.Errorf("channels = %d, want 5 (preserved when channelConfiguration == 0)", tr.channels)
	}
}

// --- extractFLAC / audioExtOffset / parseAudioFields / parseESDS ---------------

// TestExtractFLACLengthBoundary kills the CONDITIONALS_BOUNDARY on
// `len(dfla) < 4` (parse.go:1645): exactly 4 bytes (version/flags only, no
// metadata blocks) is a valid, minimal dfLa; 3 bytes must be rejected.
func TestExtractFLACLengthBoundary(t *testing.T) {
	var tr inTrack
	dfla4 := box("dfLa", []byte{0, 0, 0, 0})
	entry := append(make([]byte, 28), dfla4...)
	if err := extractFLAC(&tr, entry, 28); err != nil {
		t.Fatalf("extractFLAC(4-byte dfLa): %v", err)
	}
	if string(tr.codecPrivate) != "fLaC" {
		t.Errorf("codecPrivate = %q, want \"fLaC\"", tr.codecPrivate)
	}

	var tr2 inTrack
	dfla3 := box("dfLa", []byte{0, 0, 0})
	entry2 := append(make([]byte, 28), dfla3...)
	if err := extractFLAC(&tr2, entry2, 28); err == nil {
		t.Error("a 3-byte dfLa must be rejected as too short")
	}
}

// TestAudioExtOffsetLengthBoundary kills the CONDITIONALS_BOUNDARY on
// `len(payload) < 10` (parse.go:1938): exactly 10 bytes is enough to read the
// version field; 9 is not (must fall back to the ISO default, 28).
func TestAudioExtOffsetLengthBoundary(t *testing.T) {
	p10 := make([]byte, 10)
	binary.BigEndian.PutUint16(p10[8:10], 1) // QuickTime SoundDescription v1
	if got := audioExtOffset(p10); got != 44 {
		t.Errorf("exactly-10-byte payload (version 1) = %d, want 44", got)
	}
	if got := audioExtOffset(p10[:9]); got != 28 {
		t.Errorf("9-byte payload = %d, want 28 (default, too short to read the version field)", got)
	}
}

// TestParseAudioFieldsLengthBoundaryAndBtrt kills the CONDITIONALS_BOUNDARY/
// CONDITIONALS_NEGATION on `len(payload) >= 28` (parse.go:1963) and the
// CONDITIONALS_NEGATION on `len(payload) >= ext` (parse.go:1967).
func TestParseAudioFieldsLengthBoundaryAndBtrt(t *testing.T) {
	p := make([]byte, 28)
	binary.BigEndian.PutUint16(p[16:18], 2)         // channels
	binary.BigEndian.PutUint32(p[24:28], 44100<<16) // sample rate (16.16)

	var tr inTrack
	parseAudioFields(&tr, p)
	if tr.channels != 2 || tr.sampleRate != 44100 {
		t.Errorf("exactly-28-byte payload = %dch/%vHz, want 2/44100", tr.channels, tr.sampleRate)
	}

	var tr2 inTrack
	parseAudioFields(&tr2, p[:27])
	if tr2.channels != 0 || tr2.sampleRate != 0 {
		t.Errorf("27-byte payload = %dch/%vHz, want 0/0 (fields left unset)", tr2.channels, tr2.sampleRate)
	}

	btrt := box("btrt", append(append(u32be(0), u32be(0)...), u32be(192000)...))
	withBtrt := append(append([]byte{}, p...), btrt...)
	var tr3 inTrack
	parseAudioFields(&tr3, withBtrt)
	if tr3.bitrate != 192000 {
		t.Errorf("bitrate = %d, want 192000 (a btrt beyond the fixed 28 bytes must be read)", tr3.bitrate)
	}
}

// TestParseESDSLengthBoundaryMessage kills the CONDITIONALS_BOUNDARY on
// `len(esds) < 4` (parse.go:1977) by checking the exact error message at the
// boundary.
func TestParseESDSLengthBoundaryMessage(t *testing.T) {
	_, _, _, err := parseESDS([]byte{0, 0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "expected ES_Descriptor") {
		t.Errorf("4-byte esds err = %v, want \"expected ES_Descriptor\"", err)
	}
	_, _, _, err = parseESDS([]byte{0, 0, 0})
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("3-byte esds err = %v, want \"too short\"", err)
	}
}
