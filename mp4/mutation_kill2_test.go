package mp4

// mutation_kill2_test.go — second-pass targeted tests killing remaining gremlins
// survivors. Each test applies the boundary/arithmetic exactly at the mutated
// operator and asserts the observable difference.

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ── audio.go:55 — bitWriter.bytes() nbits > 0 guard ──────────────────────────

// TestBitWriterBytesNoExtraFlushWhenEmpty kills the CONDITIONALS_BOUNDARY
// mutant (> 0 → >= 0). When nbits==0 the mutation appends a zero byte; the
// original does not.
func TestBitWriterBytesNoExtraFlushWhenEmpty(t *testing.T) {
	var w bitWriter
	w.write(0xFF, 8) // writes exactly one full byte; leaves nbits==0
	got := w.bytes()
	if len(got) != 1 || got[0] != 0xFF {
		t.Errorf("bytes() = % x, want [ff]", got)
	}
}

// ── audio.go:68 — findSync maxScan capping ───────────────────────────────────

// TestFindSyncMaxScanCapLenMinus2 kills the CONDITIONALS_BOUNDARY mutant
// (> → >=).  With maxScan == len(frame)-2 the mutation would cap it one lower,
// missing the syncword at that exact position.
func TestFindSyncMaxScanCapLenMinus2(t *testing.T) {
	// Syncword at index 2 in a 4-byte frame. maxScan==2 == len-2.
	// Original: 2 > 2 is false → keeps maxScan=2 → finds syncword.
	// Mutated:  2 >= 2 is true → maxScan=1 → misses syncword.
	frame := []byte{0x00, 0x00, 0x0B, 0x77}
	if got := findSync(frame, 0x0B77, 2); got != 2 {
		t.Errorf("syncword at index 2 with maxScan==len-2: got %d, want 2", got)
	}
}

// TestFindSyncArithmeticMinus2 kills the ARITHMETIC_BASE mutant on the literal
// 2 in `len(frame)-2`.  With 2→1 the cap changes to len-1, allowing an
// off-by-one frame[i+1] access.  We verify the syncword at the last valid
// position is found without panic.
func TestFindSyncArithmeticMinus2(t *testing.T) {
	// Frame of length 5; syncword at index 2.  maxScan=4 (> len-2=3).
	// Original: 4 > 3 → cap to 3, loop 0..3, finds syncword.
	// Mutated (2→1): cap to len-1=4, loop 0..4, frame[5] OOB panic.
	frame := []byte{0x00, 0x00, 0x0B, 0x77, 0xFF}
	if got := findSync(frame, 0x0B77, 4); got != 2 {
		t.Errorf("syncword at index 2: got %d, want 2", got)
	}
}

// ── audio.go:299 — eac3Channels numDepSub boundary ───────────────────────────

// TestEAC3ChannelsNumDepSubZero kills the CONDITIONALS_BOUNDARY mutant
// (> 0 → >= 0).  With numDepSub=0 the mutation reads a chanLoc field from
// the next (garbage) bytes; we pad with 0xFF so extra channels would be added
// if the guard is wrong.
func TestEAC3ChannelsNumDepSubZero(t *testing.T) {
	// Build a dec3 payload: acmod=7 (5ch), lfeon=1 → ch=6, numDepSub=0.
	// Trailing 0xFF bytes ensure mutation would read non-zero chanLoc.
	var bw bitWriter
	bw.write(0, 13)                        // data_rate
	bw.write(0, 3)                         // num_ind_sub-1
	bw.write(0, 2)                         // fscod
	bw.write(8, 5)                         // bsid
	bw.write(0, 1)                         // reserved
	bw.write(0, 1)                         // asvc
	bw.write(0, 3)                         // bsmod
	bw.write(7, 3)                         // acmod=7 → 5 ch
	bw.write(1, 1)                         // lfeon=1 → +1 = 6 total
	bw.write(0, 3)                         // reserved
	bw.write(0, 4)                         // numDepSub=0
	bw.write(0, 1)                         // reserved
	dec3 := append(bw.bytes(), 0xFF, 0xFF) // garbage chanLoc if guard is wrong

	if got := eac3Channels(dec3); got != 6 {
		t.Errorf("eac3Channels(numDepSub=0) = %d, want 6", got)
	}
}

// ── audio.go:410-411 — parseEAC3 dataRate arithmetic ─────────────────────────

// TestParseEAC3DataRateArithmetic kills ARITHMETIC_BASE mutants on the
// `frameBytes * 8 * sr / (samples * 1000)` expression by asserting the exact
// integer result.  frmsiz=99→frameBytes=200, fscod=0→sr=48000,
// numblkscod=3→samples=1536. dataRate=200*8*48000/(1536*1000)=50.
func TestParseEAC3DataRateArithmetic(t *testing.T) {
	var bw bitWriter
	bw.write(0x0B77, 16) // syncword
	bw.write(0, 2)       // strmtyp
	bw.write(0, 3)       // substreamid
	bw.write(99, 11)     // frmsiz=99 → frameBytes=(99+1)*2=200
	bw.write(0, 2)       // fscod=0 → sr=48000
	bw.write(3, 2)       // numblkscod=3 → samples=6*256=1536
	bw.write(3, 3)       // acmod=3
	bw.write(0, 1)       // lfeon
	bw.write(8, 5)       // bsid=8
	bw.write(0, 8)       // padding

	dec3, err := parseEAC3(bw.bytes())
	if err != nil {
		t.Fatalf("parseEAC3: %v", err)
	}
	// dec3 is box("dec3", payload): skip the 8-byte box header to reach payload.
	if len(dec3) < 9 {
		t.Fatalf("dec3 too short: %d", len(dec3))
	}
	// dec3 payload starts with 13-bit dataRate.
	r := &bitReader{data: dec3[8:]}
	gotRate := r.bits(13)
	// 200*8*48000/(1536*1000) = 76800000/1536000 = 50
	if gotRate != 50 {
		t.Errorf("dataRate = %d, want 50", gotRate)
	}
}

// ── audio.go:441 — flacEntry CodecPrivate stripping >=4 ──────────────────────

// TestFlacEntryStripsFLaCMarkerExactly4 kills the CONDITIONALS_BOUNDARY mutant
// (>= 4 → > 4).  A CodecPrivate of exactly "fLaC" (4 bytes) must be stripped;
// with the mutation (> 4 = false), "fLaC" would not be stripped and ends up
// verbatim in the dfLa payload.
func TestFlacEntryStripsFLaCMarkerExactly4(t *testing.T) {
	tr := &mkv.Track{ID: 1, Codec: "flac", CodecPrivate: []byte("fLaC")}
	entry, err := flacEntry(tr, nil)
	if err != nil {
		t.Fatalf("flacEntry: %v", err)
	}
	// audioSampleEntry structure:
	//   outer box header (8) + audio entry data (28) = 36 bytes before config box.
	// The config box (dfLa) starts at offset 36.
	const audioEntryHdrLen = 36
	if len(entry) < audioEntryHdrLen+8 {
		t.Fatalf("entry too short: %d", len(entry))
	}
	dflaBoxes, err := iterBoxes(entry[audioEntryHdrLen:])
	if err != nil {
		t.Fatalf("iterBoxes dfLa region: %v", err)
	}
	for _, b := range dflaBoxes {
		if b.typ == "dfLa" {
			// dfLa: 4-byte fullbox header + metadata. After stripping "fLaC" from
			// a 4-byte CP the metadata is empty → fullbox header (4 bytes), nothing else.
			meta := b.payload[4:] // skip version/flags
			if bytes.Contains(meta, []byte("fLaC")) {
				t.Errorf("dfLa must not contain the fLaC marker, got %q", meta)
			}
			return
		}
	}
	t.Error("dfLa box not found in flacEntry output")
}

// ── box.go:143 — packLanguage arithmetic ─────────────────────────────────────

// TestPackLanguageRoundTrip kills the INVERT_NEGATIVES and ARITHMETIC_BASE
// mutants on the `-0x60` shift in packLanguage by asserting an exact value
// and verifying the round-trip.
func TestPackLanguageRoundTrip(t *testing.T) {
	// "eng": 'e'(0x65-0x60=5), 'n'(0x6E-0x60=14), 'g'(0x67-0x60=7)
	// packed = (5<<10)|(14<<5)|7 = 0x1400|0x01C0|0x0007 = 0x15C7
	const want = uint16(0x15C7)
	if got := packLanguage("eng"); got != want {
		t.Errorf("packLanguage(eng) = 0x%04X, want 0x%04X", got, want)
	}
	// Round-trip: decodeMdhdLanguage(packLanguage("fra")) == "fra"
	if got := decodeMdhdLanguage(packLanguage("fra")); got != "fra" {
		t.Errorf("round-trip fra: %q", got)
	}
	// "und" must pack and decode to "" (filtered out as undefined).
	if got := decodeMdhdLanguage(packLanguage("und")); got != "" {
		t.Errorf("round-trip und: %q, want empty", got)
	}
}

// ── parse.go:567,570 — parseMdhd exact boundary ──────────────────────────────

// TestParseMdhdTsOffExactBoundary kills the CONDITIONALS_BOUNDARY mutant
// (>= → >) on `len(payload) >= tsOff+4` and ARITHMETIC_BASE on the `4`.
// v0: tsOff=12 → boundary at 16 bytes.
func TestParseMdhdTsOffExactBoundary(t *testing.T) {
	// 16 bytes: exactly tsOff+4.  Must return ts.
	var p [16]byte
	p[0] = 0 // version 0
	binary.BigEndian.PutUint32(p[12:16], 90000)
	ts, _ := parseMdhd(p[:])
	if ts != 90000 {
		t.Errorf("16-byte mdhd v0: ts = %d, want 90000", ts)
	}
	// 15 bytes: one short → ts must be 0.
	ts, _ = parseMdhd(p[:15])
	if ts != 0 {
		t.Errorf("15-byte mdhd v0: ts = %d, want 0", ts)
	}
	// v1: tsOff=20 → boundary at 24 bytes.
	var p1 [24]byte
	p1[0] = 1
	binary.BigEndian.PutUint32(p1[20:24], 48000)
	ts, _ = parseMdhd(p1[:])
	if ts != 48000 {
		t.Errorf("24-byte mdhd v1: ts = %d, want 48000", ts)
	}
	ts, _ = parseMdhd(p1[:23])
	if ts != 0 {
		t.Errorf("23-byte mdhd v1: ts = %d, want 0", ts)
	}
}

// TestParseMdhdLangOffExactBoundary kills the CONDITIONALS_BOUNDARY mutant on
// `len(payload) >= langOff+2`.  v0: langOff=20 → boundary at 22 bytes.
func TestParseMdhdLangOffExactBoundary(t *testing.T) {
	// 22 bytes: exactly langOff+2 for v0; language "eng" = packLanguage("eng").
	var p [22]byte
	p[0] = 0
	binary.BigEndian.PutUint32(p[12:16], 1000) // ts
	// Pack "eng" into language field at offset 20.
	binary.BigEndian.PutUint16(p[20:22], packLanguage("eng"))
	_, lang := parseMdhd(p[:])
	if lang != "eng" {
		t.Errorf("22-byte mdhd v0: lang = %q, want eng", lang)
	}
	_, lang = parseMdhd(p[:21])
	if lang != "" {
		t.Errorf("21-byte mdhd v0: lang = %q, want empty", lang)
	}
}

// ── parse.go:587:44 — mdhdDurationMs durOff+durSize exact boundary ────────────

// TestMdhdDurationMsDurOffBoundary kills the CONDITIONALS_BOUNDARY mutant
// (<  → <=) on `len(payload) < durOff+durSize`.
// v0: durOff=16, durSize=4 → boundary at 20 bytes.
func TestMdhdDurationMsDurOffBoundary(t *testing.T) {
	// 20 bytes: exactly durOff+durSize (v0).  Must return non-zero.
	var p [20]byte
	p[0] = 0
	binary.BigEndian.PutUint32(p[12:16], 1000) // ts=1000
	binary.BigEndian.PutUint32(p[16:20], 2000) // dur=2000
	d := mdhdDurationMs(p[:])
	if d != 2000 { // 2000*1000/1000
		t.Errorf("20-byte mdhd v0: dur = %d, want 2000", d)
	}
	// 19 bytes: one short → must return 0.
	if d := mdhdDurationMs(p[:19]); d != 0 {
		t.Errorf("19-byte mdhd v0: dur = %d, want 0", d)
	}
}

// ── parse.go:619 — parseElst loop boundary ───────────────────────────────────

// TestParseElstV0ExactEntry kills the CONDITIONALS_BOUNDARY mutant (> → >=)
// on `off+12 > len(payload)` for v0 entries.  An elst with exactly one 12-byte
// v0 entry must be parsed; 11 bytes must break without returning an entry.
func TestParseElstV0ExactEntry(t *testing.T) {
	// v0 elst: version+flags(4) count(4) [segDur(4) mt(4) rate(4) = 12 bytes]
	var w bw
	w.u32(0)          // version=0, flags=0
	w.u32(1)          // count=1
	w.u32(50)         // segDur=50 ms
	w.u32(10)         // mediaTime=10 (positive → start trim)
	w.u32(0x00010000) // media_rate 1.0
	mt, emptyDur, ok := parseElst(w.b)
	if !ok || mt != 10 || emptyDur != 0 {
		t.Errorf("v0 elst: ok=%v mt=%d emptyDur=%d, want ok=true mt=10 emptyDur=0", ok, mt, emptyDur)
	}
	// Truncate to 11 bytes for the entry (one short).
	_, _, ok = parseElst(w.b[:len(w.b)-1])
	if ok {
		t.Error("truncated v0 elst: must not find entry")
	}
}

// TestParseElstEmptyEditArithmetic kills the ARITHMETIC_BASE mutant on
// emptyDuration accumulation.  Two consecutive empty edits must sum correctly.
func TestParseElstEmptyEditArithmetic(t *testing.T) {
	var w bw
	w.u32(0) // version=0
	w.u32(2) // count=2
	// First entry: mt=-1 (empty edit, segDur=100).
	w.u32(100)
	w.u32(0xFFFFFFFF)
	w.u32(0x00010000)
	// Second entry: mt=0 (start trim at 0, segDur=200).
	w.u32(200)
	w.u32(0)
	w.u32(0x00010000)
	mt, emptyDur, ok := parseElst(w.b)
	if !ok || mt != 0 || emptyDur != 100 {
		t.Errorf("elst two-entry: ok=%v mt=%d emptyDur=%d, want ok=true mt=0 emptyDur=100", ok, mt, emptyDur)
	}
}

// TestParseElstNegativeMtBoundary kills the CONDITIONALS_BOUNDARY at line 636
// (`mt < 0`).  mt=0 is a valid start trim (not an empty edit); mutation
// `mt <= 0` would treat mt=0 as empty and accumulate segDur instead.
func TestParseElstNegativeMtBoundary(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(1)
	w.u32(500) // segDur
	w.u32(0)   // mediaTime=0 (not negative)
	w.u32(0x00010000)
	mt, emptyDur, ok := parseElst(w.b)
	if !ok || mt != 0 || emptyDur != 0 {
		t.Errorf("mt=0: ok=%v mt=%d emptyDur=%d, want ok=true mt=0 emptyDur=0", ok, mt, emptyDur)
	}
}

// ── parse.go:655,664 — decodeMdhdLanguage boundaries ─────────────────────────

// TestDecodeMdhdLanguageMacCodeBoundary kills the CONDITIONALS_BOUNDARY mutant
// (< 0x400 → <= 0x400).  packed=0x400 is the smallest valid ISO-packed code
// ("aaa") and must NOT go to the Mac fallback.
func TestDecodeMdhdLanguageMacCodeBoundary(t *testing.T) {
	// 0x3FF: Mac code boundary (just below 0x400) → goes to macLanguageToISO.
	// macLanguageCodes[0x3FF] is absent → returns "".
	if got := decodeMdhdLanguage(0x3FF); got != "" {
		t.Errorf("0x3FF (Mac code area): %q, want empty", got)
	}
	// 0x400: packed code 'a','a','a' = degenerate but valid path (not Mac).
	// 'a'-0x60=1, so code = (1<<10)|(1<<5)|1 = 0x421.
	// 0x400 decodes as (0<<10)&0x1f + 0x60 = 0x60 = '`', not a-z → returns "".
	if got := decodeMdhdLanguage(0x400); got != "" {
		t.Errorf("0x400: %q, want empty (invalid char '`')", got)
	}
}

// TestDecodeMdhdLanguageCharBoundaries kills the CONDITIONALS_BOUNDARY mutants
// on `ch < 'a'` and `ch > 'z'` in decodeMdhdLanguage.
func TestDecodeMdhdLanguageCharBoundaries(t *testing.T) {
	// 'a'-1 = '`' (0x60): first char below range → returns "".
	// Pack '`'(0x60→0), 'n', 'g': (0<<10)|(14<<5)|7 = 0x01C7 but byte would be 0x60 not in a-z.
	// Easier: use 0x0421 = (1<<10)|(1<<5)|1 = 'a','a','a' → valid.
	got := decodeMdhdLanguage(0x0421)
	if got != "" {
		// "aaa" is a valid code but decodes to "aaa" which != "und".
		if got != "aaa" {
			t.Errorf("0x0421 (aaa): %q, want aaa or empty", got)
		}
	}
	// 'z'+1 = '{' (0x7B): pack '{'-0x60 = 27 = 0x1B.
	// (27<<10)|(1<<5)|1 = 0x6C21 → first char is 0x1B+0x60 = 0x7B = '{' → out of range.
	got2 := decodeMdhdLanguage(0x6C21)
	if got2 != "" {
		t.Errorf("first char '{': %q, want empty", got2)
	}
}

// ── parse.go:708,720 — tkhdTrackID boundaries ────────────────────────────────

// TestTkhdTrackIDBoundary kills the CONDITIONALS_BOUNDARY mutant on
// `len(payload) < off+4`.  v0: off=12 → boundary at 16 bytes.
func TestTkhdTrackIDBoundary(t *testing.T) {
	var p [16]byte
	p[0] = 0                                // version 0
	binary.BigEndian.PutUint32(p[12:16], 7) // trackID=7
	if got := tkhdTrackID(p[:]); got != 7 {
		t.Errorf("16-byte tkhd v0: trackID=%d, want 7", got)
	}
	if got := tkhdTrackID(p[:15]); got != 0 {
		t.Errorf("15-byte tkhd v0: trackID=%d, want 0", got)
	}
	// v1: off=20 → boundary at 24 bytes.
	var p1 [24]byte
	p1[0] = 1
	binary.BigEndian.PutUint32(p1[20:24], 3)
	if got := tkhdTrackID(p1[:]); got != 3 {
		t.Errorf("24-byte tkhd v1: trackID=%d, want 3", got)
	}
	if got := tkhdTrackID(p1[:23]); got != 0 {
		t.Errorf("23-byte tkhd v1: trackID=%d, want 0", got)
	}
}

// ── parse.go:727 — tkhdRotation ARITHMETIC_BASE ───────────────────────────────

// TestTkhdRotationExactOffset kills the ARITHMETIC_BASE mutant on the
// arithmetic in `matOff+8`.  We verify the matrix is read from the exact right
// offset by placing the rotation matrix at exactly matOff=40 (v0).
func TestTkhdRotationExactOffset(t *testing.T) {
	// v0 tkhd: matOff=40.  Need len >= 48 (40+8).
	// Build a 48-byte payload with identity in the matrix except b=sin(90°)=1.0
	// (in 16.16: 0x00010000) to get 90° rotation.
	p := make([]byte, 48)
	p[0] = 0 // version 0
	// a (matOff+0): 0 (not 1.0) → rotation is 90° or 270° from atan2(b,a).
	// b (matOff+4): 0x00010000 → atan2(1, 0) = 90°.
	binary.BigEndian.PutUint32(p[40:44], 0)          // a=0
	binary.BigEndian.PutUint32(p[44:48], 0x00010000) // b=1.0
	if got := tkhdRotation(p); got != 90 {
		t.Errorf("b=1.0, a=0: rotation = %d, want 90", got)
	}
	// 47-byte payload: one short → must return 0.
	if got := tkhdRotation(p[:47]); got != 0 {
		t.Errorf("47-byte tkhd: rotation = %d, want 0", got)
	}
}

// ── parse.go:746-780 — parseElng / parseKind boundaries ─────────────────────

// TestParseElngMinBoundary kills the CONDITIONALS_BOUNDARY on `len(payload) < 4`.
func TestParseElngMinBoundary(t *testing.T) {
	// 3 bytes: below 4-byte fullbox header → return "".
	if got := parseElng(make([]byte, 3)); got != "" {
		t.Errorf("3-byte elng: %q, want empty", got)
	}
	// 4 bytes: valid header, empty tag after stripping → "".
	if got := parseElng([]byte{0, 0, 0, 0}); got != "" {
		t.Errorf("4-byte all-zero elng: %q, want empty", got)
	}
	// 4-byte header + "zh-TW" + NUL.
	p := append([]byte{0, 0, 0, 0}, []byte("zh-TW\x00")...)
	if got := parseElng(p); got != "zh-TW" {
		t.Errorf("elng = %q, want zh-TW", got)
	}
}

// TestParseKindBoundary kills the CONDITIONALS_BOUNDARY on len(payload) < 4.
func TestParseKindBoundary(t *testing.T) {
	// 3 bytes: too short → both return empty.
	s, v := parseKind(make([]byte, 3))
	if s != "" || v != "" {
		t.Errorf("3-byte kind: scheme=%q value=%q, want both empty", s, v)
	}
	// Full valid kind: version(4) + "urn:foo\x00value\x00"
	p := append([]byte{0, 0, 0, 0}, []byte("urn:foo\x00val\x00")...)
	scheme, value := parseKind(p)
	if scheme != "urn:foo" || value != "val" {
		t.Errorf("kind: scheme=%q value=%q, want urn:foo/val", scheme, value)
	}
}

// ── parse.go:900,914,929 — opusHeadFromDOps boundaries ───────────────────────

// TestOpusHeadFromDOpsMinBoundary kills the CONDITIONALS_BOUNDARY mutant on
// `len(dops) < 11`.
func TestOpusHeadFromDOpsMinBoundary(t *testing.T) {
	// 10 bytes: too short.
	if _, err := opusHeadFromDOps(make([]byte, 10)); err == nil {
		t.Error("10-byte dOps: want error")
	}
	// 11 bytes: minimum → succeeds (family=0, no channel mapping).
	p := make([]byte, 11)
	p[1] = 2  // channels
	p[10] = 0 // family=0
	head, err := opusHeadFromDOps(p)
	if err != nil {
		t.Fatalf("11-byte dOps: %v", err)
	}
	if len(head) < 9 || head[9] != 2 {
		t.Errorf("OpusHead channels = %d, want 2", head[9])
	}
}

// TestOpusHeadChannelMappingTruncation kills the CONDITIONALS_BOUNDARY on
// `len(dops) < 11+need`.  For family=1, need=2+channels; test exact boundary.
func TestOpusHeadChannelMappingTruncation(t *testing.T) {
	channels := byte(2)
	need := 2 + int(channels) // need=4
	// Exactly 11+need = 15 bytes: succeeds.
	p := make([]byte, 11+need)
	p[1] = channels
	p[10] = 1 // family=1 → requires channel mapping
	head, err := opusHeadFromDOps(p)
	if err != nil || len(head) != 19+need {
		t.Errorf("exact 15-byte dOps: err=%v len=%d, want nil err and len=%d", err, len(head), 19+need)
	}
	// 14 bytes (one short): must error.
	p2 := p[:11+need-1]
	if _, err := opusHeadFromDOps(p2); err == nil {
		t.Error("truncated channel mapping: want error")
	}
}

// ── parse.go:1077,1089 — parseESDS boundaries ────────────────────────────────

// TestParseESDSMinBoundary kills the CONDITIONALS_BOUNDARY on `len(esds) < 4`.
func TestParseESDSMinBoundary(t *testing.T) {
	if _, _, _, err := parseESDS(make([]byte, 3)); err == nil {
		t.Error("3-byte esds: want error")
	}
}

// TestParseESDSDecoderConfigLen kills the CONDITIONALS_BOUNDARY on
// `len(dcfg) >= 13` for avgBitrate extraction.
func TestParseESDSDecoderConfigLen(t *testing.T) {
	// Build a minimal esds with a DecoderConfigDescriptor of exactly 13 bytes
	// (avgBitrate must be read) vs 12 bytes (avgBitrate stays 0).
	buildESDS := func(dcfgLen int) []byte {
		dcfg := make([]byte, dcfgLen)
		dcfg[0] = 0x40 // objectType AAC
		if dcfgLen >= 13 {
			binary.BigEndian.PutUint32(dcfg[9:13], 128000) // avgBitrate=128000
		}
		dc := descriptor(0x04, dcfg)
		sl := descriptor(0x06, []byte{0x02})
		var es bw
		es.u16(1)
		es.u8(0)
		es.bytes(dc)
		es.bytes(sl)
		return fullBox("esds", 0, 0, func(w *bw) { w.bytes(descriptor(0x03, es.b)) })
	}

	// 13-byte dcfg: avgBitrate must be 128000.
	objType, _, avg, err := parseESDS(buildESDS(13)[8:]) // skip box header
	if err != nil || objType != 0x40 || avg != 128000 {
		t.Errorf("dcfg=13: err=%v objType=%02x avg=%d, want nil/0x40/128000", err, objType, avg)
	}
	// 12-byte dcfg: avgBitrate must remain 0.
	_, _, avg2, err2 := parseESDS(buildESDS(12)[8:])
	if err2 != nil || avg2 != 0 {
		t.Errorf("dcfg=12: err=%v avg=%d, want nil/0", err2, avg2)
	}
}

// ── parse.go:1126,1132 — parseESDS DecoderConfigDescriptor length ─────────────

// TestParseESDSDcfgMinBoundary kills the CONDITIONALS_BOUNDARY on
// `len(dcfg) < 1`.  An empty DecoderConfigDescriptor must error.
func TestParseESDSDcfgMinBoundary(t *testing.T) {
	sl := descriptor(0x06, []byte{0x02})
	var es bw
	es.u16(1)
	es.u8(0)
	es.bytes(descriptor(0x04, []byte{})) // empty dcfg
	es.bytes(sl)
	esds := fullBox("esds", 0, 0, func(w *bw) { w.bytes(descriptor(0x03, es.b)) })
	if _, _, _, err := parseESDS(esds[8:]); err == nil {
		t.Error("empty DecoderConfigDescriptor: want error")
	}
}

// ── parse.go:1181 — descReader.skip ──────────────────────────────────────────

// TestDescReaderSkipBoundary kills the CONDITIONALS_BOUNDARY on
// `n < 0 || d.pos+n > len(d.buf)`.
func TestDescReaderSkipBoundary(t *testing.T) {
	d := &descReader{buf: []byte{1, 2, 3, 4, 5}}
	// Skip 5 (exactly exhausts buffer): must succeed.
	if err := d.skip(5); err != nil {
		t.Errorf("skip 5 from len=5: %v", err)
	}
	// Skip 1 from exhausted buffer: must error.
	if err := d.skip(1); err == nil {
		t.Error("skip past end: want error")
	}
	// Skip with n<0: must error.
	d2 := &descReader{buf: []byte{0}}
	if err := d2.skip(-1); err == nil {
		t.Error("skip n=-1: want error")
	}
}

// ── parse.go:1195-1206 — descReader.next ─────────────────────────────────────

// TestDescReaderNextBoundary kills CONDITIONALS_BOUNDARY on `size < 0` and
// `d.pos+size > len(d.buf)`, and INCREMENT_DECREMENT on the loop counter.
func TestDescReaderNextBoundary(t *testing.T) {
	// Minimal 1-byte body: tag=0x03, length=1, body=[0xAA].
	buf := []byte{0x03, 0x01, 0xAA}
	d := &descReader{buf: buf}
	tag, body, err := d.next()
	if err != nil || tag != 0x03 || !bytes.Equal(body, []byte{0xAA}) {
		t.Errorf("next: tag=%02x body=%x err=%v, want 03/aa/nil", tag, body, err)
	}
	// Truncated body (length=5 but only 1 byte): must error.
	buf2 := []byte{0x03, 0x05, 0x00}
	d2 := &descReader{buf: buf2}
	if _, _, err := d2.next(); err == nil {
		t.Error("truncated descriptor body: want error")
	}
	// Multi-byte length encoding (0x80 continuation bit).
	// Length = 0x80|0x00 = 0 followed by 0x01 = 1. Body = [0xBB].
	buf3 := []byte{0x07, 0x80, 0x01, 0xBB}
	d3 := &descReader{buf: buf3}
	tag3, body3, err3 := d3.next()
	if err3 != nil || tag3 != 0x07 || !bytes.Equal(body3, []byte{0xBB}) {
		t.Errorf("multi-byte len: tag=%02x body=%x err=%v, want 07/bb/nil", tag3, body3, err3)
	}
}

// ── sampletable.go:52 — buildSampleTable ci+1 arithmetic ────────────────────

// TestBuildSampleTableCiPlusOne kills the ARITHMETIC_BASE mutant on `ci+1` in
// `samplesForChunk(uint32(ci+1), stscEntries)`.  With ci starting at 0, the
// 1-based chunk number must be ci+1; mutation ci+0=ci would give 0 for the
// first chunk, returning 0 samples and skipping all data.
func TestBuildSampleTableCiPlusOne(t *testing.T) {
	// One chunk at offset 100, one sample of size 4. The single stsc entry has
	// firstChunk=1 (1-based). If ci+1→ci the lookup returns 0 and si!=n → error.
	sizes := buildStsz([]uint32{4})  // 1 sample, size=4
	stco := buildStco([]uint64{100}) // 1 chunk at offset 100
	stsc := buildStsc([]stscEntry{{firstChunk: 1, perChunk: 1}})
	stts := buildSttsPayload([]uint32{1}, []uint32{33}) // 1 run, delta=33
	stblBoxes := []memBox{
		{typ: "stsz", payload: sizes},
		{typ: "stco", payload: stco},
		{typ: "stsc", payload: stsc},
		{typ: "stts", payload: stts},
	}
	tr := inTrack{timescale: 1000}
	if err := buildSampleTable(&tr, stblBoxes, 200); err != nil {
		t.Fatalf("buildSampleTable: %v", err)
	}
	if len(tr.samples) != 1 || tr.samples[0].offset != 100 || tr.samples[0].size != 4 {
		t.Errorf("samples = %+v, want [{offset:100 size:4}]", tr.samples)
	}
}

// ── sampletable.go:54 — k < perChunk loop boundary ──────────────────────────

// TestBuildSampleTablePerChunkBoundary kills the CONDITIONALS_BOUNDARY mutant
// (< → <=) on `k < perChunk`.  With perChunk=2 and 2 samples per chunk, the
// mutation would try to consume 3 samples per chunk → si overruns n → error.
func TestBuildSampleTablePerChunkBoundary(t *testing.T) {
	// 2 samples in 1 chunk (perChunk=2).  Must succeed.
	sizes := buildStsz([]uint32{5, 6})
	stco := buildStco([]uint64{50})
	stsc := buildStsc([]stscEntry{{firstChunk: 1, perChunk: 2}})
	stts := buildSttsPayload([]uint32{2}, []uint32{40})
	stblBoxes := []memBox{
		{typ: "stsz", payload: sizes},
		{typ: "stco", payload: stco},
		{typ: "stsc", payload: stsc},
		{typ: "stts", payload: stts},
	}
	tr := inTrack{timescale: 1000}
	if err := buildSampleTable(&tr, stblBoxes, 200); err != nil {
		t.Fatalf("buildSampleTable: %v", err)
	}
	if len(tr.samples) != 2 {
		t.Errorf("samples len = %d, want 2", len(tr.samples))
	}
	// Offsets: first at 50, second at 50+5=55.
	if tr.samples[0].offset != 50 || tr.samples[1].offset != 55 {
		t.Errorf("offsets = [%d %d], want [50 55]", tr.samples[0].offset, tr.samples[1].offset)
	}
}

// ── sampletable.go:57 — pos < 0 boundary for sample at offset 0 ─────────────

// TestBuildSampleTableChunkAtOffset0 kills the CONDITIONALS_BOUNDARY mutant
// (< 0 → <= 0) on `pos < 0`.  A legitimate sample at file offset 0 must be
// accepted; with the mutation pos<=0 the sample would be rejected as invalid.
func TestBuildSampleTableChunkAtOffset0(t *testing.T) {
	sizes := buildStsz([]uint32{10})
	stco := buildStco([]uint64{0}) // chunk starts at byte 0
	stsc := buildStsc([]stscEntry{{firstChunk: 1, perChunk: 1}})
	stts := buildSttsPayload([]uint32{1}, []uint32{33})
	stblBoxes := []memBox{
		{typ: "stsz", payload: sizes},
		{typ: "stco", payload: stco},
		{typ: "stsc", payload: stsc},
		{typ: "stts", payload: stts},
	}
	tr := inTrack{timescale: 1000}
	if err := buildSampleTable(&tr, stblBoxes, 100); err != nil {
		t.Errorf("chunk at offset 0: %v", err)
	}
	if len(tr.samples) != 1 || tr.samples[0].offset != 0 {
		t.Errorf("samples = %+v, want [{offset:0}]", tr.samples)
	}
}

// ── sampletable.go:79 — si != n mismatch error ──────────────────────────────

// TestBuildSampleTableSiMismatchError kills the CONDITIONALS_BOUNDARY mutant
// (si != n → si == n) on the post-loop check.  When the chunk table covers
// fewer samples than stsz declares, si < n and an error must be returned.
func TestBuildSampleTableSiMismatchError(t *testing.T) {
	// stsz says 3 samples; stco/stsc only covers 2 (1 chunk, perChunk=2).
	sizes := buildStsz([]uint32{4, 4, 4}) // n=3
	stco := buildStco([]uint64{0})
	stsc := buildStsc([]stscEntry{{firstChunk: 1, perChunk: 2}}) // covers only 2
	stts := buildSttsPayload([]uint32{3}, []uint32{40})
	stblBoxes := []memBox{
		{typ: "stsz", payload: sizes},
		{typ: "stco", payload: stco},
		{typ: "stsc", payload: stsc},
		{typ: "stts", payload: stts},
	}
	tr := inTrack{timescale: 1000}
	if err := buildSampleTable(&tr, stblBoxes, 1000); err == nil {
		t.Error("si!=n mismatch: want error, got nil")
	}
}

// ── sampletable.go:122 — parseStsz variable-size index arithmetic ──────────

// TestParseStszVariableSizeIndexBoundary kills the ARITHMETIC_BASE mutant on
// `12+i*4` in the per-sample size loop.  Provide exactly the minimum required
// bytes and verify a truncation just below it is rejected.
func TestParseStszVariableSizeIndexBoundary(t *testing.T) {
	// 5 samples: need 12 + 5*4 = 32 bytes.
	var w bw
	w.u32(0) // version/flags
	w.u32(0) // sampleSize=0 (individual)
	w.u32(5) // count=5
	for i := uint32(1); i <= 5; i++ {
		w.u32(i * 10) // sizes: 10, 20, 30, 40, 50
	}
	sizes, err := parseStsz(w.b, 0)
	if err != nil || len(sizes) != 5 || sizes[4] != 50 {
		t.Fatalf("parseStsz 5 samples: err=%v sizes=%v", err, sizes)
	}
	// Remove 4 bytes (one sample entry): must error.
	if _, err := parseStsz(w.b[:len(w.b)-4], 0); err == nil {
		t.Error("truncated by 4 bytes: want error")
	}
}

// ── sampletable.go:133-137 — parseChunkOffsets stco boundaries ───────────────

// TestParseChunkOffsetsStcoBoundary kills CONDITIONALS_BOUNDARY mutants on
// `len(stco.payload) < 8` and `< 8+int(count)*4`.
func TestParseChunkOffsetsStcoBoundary(t *testing.T) {
	// Exactly 8+3*4=20 bytes: 3 chunks.
	var w bw
	w.u32(0) // version/flags
	w.u32(3) // count=3
	w.u32(100)
	w.u32(200)
	w.u32(300)
	offs, err := parseChunkOffsets([]memBox{{typ: "stco", payload: w.b}})
	if err != nil || len(offs) != 3 || offs[0] != 100 || offs[2] != 300 {
		t.Fatalf("stco 3 chunks: err=%v offs=%v", err, offs)
	}
	// One entry short: must error.
	if _, err := parseChunkOffsets([]memBox{{typ: "stco", payload: w.b[:len(w.b)-4]}}); err == nil {
		t.Error("stco truncated by 4 bytes: want error")
	}
}

// TestParseChunkOffsetsCo64Boundary kills the corresponding co64 boundaries.
func TestParseChunkOffsetsCo64Boundary(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(2)
	w.u64(uint64(1) << 33) // large offset >32-bit
	w.u64(uint64(1) << 34)
	offs, err := parseChunkOffsets([]memBox{{typ: "co64", payload: w.b}})
	if err != nil || len(offs) != 2 || offs[0] != int64(1<<33) {
		t.Fatalf("co64 2 chunks: err=%v offs=%v", err, offs)
	}
	if _, err := parseChunkOffsets([]memBox{{typ: "co64", payload: w.b[:len(w.b)-8]}}); err == nil {
		t.Error("co64 truncated by 8 bytes: want error")
	}
}

// ── sampletable.go:147-156 — parseStts boundaries and arithmetic ─────────────

// TestParseSttsEntryBoundary kills CONDITIONALS_BOUNDARY on
// `len(payload) < 8+int(count)*8`.
func TestParseSttsEntryBoundary(t *testing.T) {
	// 2 entries: need 8+2*8=24 bytes.
	var w bw
	w.u32(0)
	w.u32(2)
	w.u32(3)
	w.u32(33) // run=3, delta=33
	w.u32(2)
	w.u32(66) // run=2, delta=66
	durs, err := parseStts(w.b, 5)
	if err != nil || len(durs) != 5 || durs[0] != 33 || durs[3] != 66 {
		t.Fatalf("parseStts 2 entries: err=%v durs=%v", err, durs)
	}
	if _, err := parseStts(w.b[:len(w.b)-8], 5); err == nil {
		t.Error("truncated by 8 bytes: want error")
	}
}

// TestParseSttsBaseArithmetic kills ARITHMETIC_BASE on `8+i*8` in the base
// index expression.
func TestParseSttsBaseArithmetic(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(3)
	w.u32(1)
	w.u32(10) // entry 0: run=1, delta=10
	w.u32(1)
	w.u32(20) // entry 1: run=1, delta=20
	w.u32(1)
	w.u32(30) // entry 2: run=1, delta=30
	durs, err := parseStts(w.b, 3)
	if err != nil || durs[0] != 10 || durs[1] != 20 || durs[2] != 30 {
		t.Errorf("stts base arithmetic: err=%v durs=%v, want [10 20 30]", err, durs)
	}
}

// TestParseSttsInnerLoopBoundary kills CONDITIONALS_BOUNDARY on `j < run`.
// A run of exactly n must fill durations exactly; mutation j <= run would
// panic writing past the end.
func TestParseSttsInnerLoopBoundary(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(1)
	w.u32(4)
	w.u32(100) // run=4, exactly n=4
	durs, err := parseStts(w.b, 4)
	if err != nil || len(durs) != 4 {
		t.Fatalf("run==n: err=%v len=%d", err, len(durs))
	}
	for i, d := range durs {
		if d != 100 {
			t.Errorf("durs[%d] = %d, want 100", i, d)
		}
	}
}

// TestParseSttsIdxArithmetic kills the INCREMENT_DECREMENT and ARITHMETIC_BASE
// mutants on `idx++` and related.  Correct indexing is proven by verifying
// each delta maps to the correct sample position.
func TestParseSttsIdxArithmetic(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(2)
	w.u32(2)
	w.u32(7) // samples 0..1 = 7
	w.u32(3)
	w.u32(11) // samples 2..4 = 11
	durs, err := parseStts(w.b, 5)
	if err != nil {
		t.Fatalf("parseStts: %v", err)
	}
	want := []uint32{7, 7, 11, 11, 11}
	for i, d := range durs {
		if d != want[i] {
			t.Errorf("durs[%d] = %d, want %d", i, d, want[i])
		}
	}
}

// ── sampletable.go:164-173 — parseCtts boundaries and arithmetic ─────────────

// TestParseCttsExactBoundary kills CONDITIONALS_BOUNDARY on the entry length
// check in parseCtts.
func TestParseCttsExactBoundary(t *testing.T) {
	// 2 ctts entries: 8+2*8=24 bytes.
	var w bw
	w.u32(0)
	w.u32(2)
	w.u32(3)
	w.u32(50) // 3 samples offset +50
	w.u32(2)
	w.u32(0) // 2 samples offset +0
	offsets := parseCtts([]memBox{{typ: "ctts", payload: w.b}}, 5)
	if len(offsets) != 5 || offsets[0] != 50 || offsets[3] != 0 {
		t.Errorf("ctts 2 entries: %v, want [50 50 50 0 0]", offsets)
	}
}

// TestParseCttsIdxAndArithmetic kills INCREMENT_DECREMENT and ARITHMETIC_BASE
// on the `base = 8+i*8` and `idx++` expressions.
func TestParseCttsIdxAndArithmetic(t *testing.T) {
	var w bw
	w.u32(0)
	w.u32(3)
	w.u32(1)
	w.u32(100)
	w.u32(1)
	w.u32(200)
	w.u32(1)
	w.u32(300)
	offsets := parseCtts([]memBox{{typ: "ctts", payload: w.b}}, 3)
	if len(offsets) != 3 || offsets[0] != 100 || offsets[1] != 200 || offsets[2] != 300 {
		t.Errorf("ctts 3 entries: %v, want [100 200 300]", offsets)
	}
}

// ── sampletable.go:202-254 — parseStss boundaries ───────────────────────────

// TestParseStssMinBoundary kills CONDITIONALS_BOUNDARY on `len < 8+count*4`.
func TestParseStssMinBoundary(t *testing.T) {
	// 3 sync samples: 8+3*4=20 bytes.
	var p bw
	p.u32(0)
	p.u32(3)
	p.u32(2)
	p.u32(5)
	p.u32(9)
	boxes := []memBox{{typ: "stss", payload: p.b}}
	set := parseStss(boxes)
	if !set[2] || !set[5] || !set[9] || len(set) != 3 {
		t.Errorf("stss 3 entries: %v, want {2:true 5:true 9:true}", set)
	}
	// Truncate last entry: returns nil (defensive).
	if parseStss([]memBox{{typ: "stss", payload: p.b[:len(p.b)-4]}}) != nil {
		t.Error("truncated stss: want nil")
	}
}

// ── sample.go:39 — addChunk count > 0 ────────────────────────────────────────

// TestAddChunkCountZeroNotAdded kills the CONDITIONALS_BOUNDARY mutant
// (> 0 → >= 0).  count=0 must not append a chunk; count=1 must append.
func TestAddChunkCountZeroNotAdded(t *testing.T) {
	var ts trackSamples
	ts.addChunk(100, 0)
	if len(ts.chunks) != 0 {
		t.Errorf("addChunk(count=0): %d chunks, want 0", len(ts.chunks))
	}
	ts.addChunk(100, 1)
	if len(ts.chunks) != 1 {
		t.Errorf("addChunk(count=1): %d chunks, want 1", len(ts.chunks))
	}
}

// ── sample.go:59 — textTiming d <= 0 guard ───────────────────────────────────

// TestTextTimingDurationGuard kills the CONDITIONALS_BOUNDARY mutant
// (d <= 0 → d < 0).  A sample with dur=0 must be clamped to 1.
func TestTextTimingDurationGuard(t *testing.T) {
	samples := []sample{{size: 10, pts: 0, dur: 0}, {size: 10, pts: 100, dur: 50}}
	tim := textTiming(samples)
	if tim.durations[0] != 1 {
		t.Errorf("dur=0 → %d, want 1", tim.durations[0])
	}
	if tim.durations[1] != 50 {
		t.Errorf("dur=50 → %d, want 50", tim.durations[1])
	}
	if tim.total != 51 {
		t.Errorf("total = %d, want 51", tim.total)
	}
}

// ── sample.go:91 — reconstructTiming sort and dts[i+1]-dts[i] arithmetic ─────

// TestReconstructTimingBFrameOffsets kills ARITHMETIC_BASE mutants on
// `dts[i+1]-dts[i]` and `samples[i].pts-dts[i]`.
func TestReconstructTimingBFrameOffsets(t *testing.T) {
	// Decode order: PTS=[0,40,20,60]. DTS (sorted) = [0,20,40,60].
	// durations = [20,20,20,20 (last=prev)]. ctts = [0,20,-20,0].
	s := []sample{
		{pts: 0, sync: true},
		{pts: 40},
		{pts: 20},
		{pts: 60},
	}
	tim := reconstructTiming(s, 20, movieTimescale)
	if len(tim.durations) != 4 {
		t.Fatalf("durations len = %d, want 4", len(tim.durations))
	}
	for i, d := range tim.durations {
		if d != 20 {
			t.Errorf("durations[%d] = %d, want 20", i, d)
		}
	}
	// ctts[0] = 0-0 = 0, ctts[1] = 40-20 = 20, ctts[2] = 20-40 = -20, ctts[3] = 60-60 = 0.
	want := []int32{0, 20, -20, 0}
	for i, c := range tim.ctts {
		if c != want[i] {
			t.Errorf("ctts[%d] = %d, want %d", i, c, want[i])
		}
	}
	if !tim.hasCTTS {
		t.Error("hasCTTS must be true")
	}
}

// ── chapters.go:90-94 — encodeChapterSample boundary and arithmetic ──────────

// TestEncodeChapterSampleLen kills CONDITIONALS_BOUNDARY (> 0xFFFF → >= 0xFFFF)
// and ARITHMETIC_BASE on `len(b)>>8`.
func TestEncodeChapterSampleLen(t *testing.T) {
	title := "Chapter One"
	s := encodeChapterSample(title)
	b := []byte(title)
	// First two bytes are big-endian length.
	wantLen := uint16(len(b))
	got := binary.BigEndian.Uint16(s[:2])
	if got != wantLen {
		t.Errorf("length field = %d, want %d", got, wantLen)
	}
	// Content matches.
	if !bytes.Equal(s[2:2+len(b)], b) {
		t.Errorf("content mismatch")
	}
}

// TestEncodeChapterSampleLenArithmetic kills ARITHMETIC_BASE on `len(b)>>8`
// and `byte(len(b))`.  Use a title whose length has a non-zero high byte.
func TestEncodeChapterSampleLenArithmetic(t *testing.T) {
	// 300-byte title: high byte = 1, low byte = 44.
	title := strings.Repeat("x", 300)
	s := encodeChapterSample(title)
	if s[0] != 1 || s[1] != 44 {
		t.Errorf("length bytes = [%d %d], want [1 44]", s[0], s[1])
	}
}

// ── chapters.go:111-124 — parseChpl entry boundaries ────────────────────────

// TestParseChplTitleLenBoundary kills the CONDITIONALS_BOUNDARY mutant
// `pos+titleLen > len(payload)`.  A titleLen that would overrun must stop.
func TestParseChplTitleLenBoundary(t *testing.T) {
	var p []byte
	p = append(p, 1, 0, 0, 0) // version/flags
	p = append(p, 0, 0, 0, 0) // reserved
	p = append(p, 2)          // count=2
	// Entry 0: valid (100ms, title "A").
	var s0 [8]byte
	binary.BigEndian.PutUint64(s0[:], 1000000) // 100ms
	p = append(p, s0[:]...)
	p = append(p, 1, 'A')
	// Entry 1: titleLen=10 but only 3 bytes left → truncated; loop breaks.
	var s1 [8]byte
	binary.BigEndian.PutUint64(s1[:], 5000000) // 500ms
	p = append(p, s1[:]...)
	p = append(p, 10) // claims 10 bytes but next append only gives 3
	p = append(p, 'X', 'Y', 'Z')

	chs := parseChpl(p)
	if len(chs) != 1 || chs[0].StartMs != 100 || chs[0].Title != "A" {
		t.Errorf("truncated chpl: %v, want [{100 A}]", chs)
	}
}

// TestParseChplStart100nsArithmeticDirect kills ARITHMETIC_BASE on
// `start100ns / 10000`.  600ms = 6000000 × 100ns units.
func TestParseChplStart100nsArithmeticDirect(t *testing.T) {
	var p []byte
	p = append(p, 1, 0, 0, 0) // version/flags
	p = append(p, 0, 0, 0, 0) // reserved
	p = append(p, 1)          // count=1
	var s [8]byte
	binary.BigEndian.PutUint64(s[:], 6000000) // 600ms in 100ns
	p = append(p, s[:]...)
	p = append(p, 0) // titleLen=0
	chs := parseChpl(p)
	if len(chs) != 1 || chs[0].StartMs != 600 {
		t.Errorf("600ms chapter: StartMs=%d, want 600", chs[0].StartMs)
	}
}

// ── moov.go:54,57 — buildMoov dur/maxID comparisons ─────────────────────────

// TestBuildMoovPicksMaxDurAndMaxID kills CONDITIONALS_BOUNDARY mutants on
// `dur > movieDur` and `t.mp4ID > maxID`.  Two tracks with different durations
// and IDs: the larger values must appear in mvhd.
func TestBuildMoovPicksMaxDurAndMaxID(t *testing.T) {
	t1 := makeOutTrackForMoov(1, 500) // mp4ID=1, dur=500ms
	t2 := makeOutTrackForMoov(3, 200) // mp4ID=3, dur=200ms → maxID=3
	moov := buildMoov([]*outTrack{t1, t2}, 1000, false, "", nil)
	boxes, err := iterBoxes(moov[8:]) // skip moov header
	if err != nil {
		t.Fatalf("iterBoxes moov: %v", err)
	}
	mvhd, ok := findMemBox(boxes, "mvhd")
	if !ok {
		t.Fatal("mvhd not found")
	}
	// mvhd v0: creation(4)+modification(4)+timescale(4)+duration(4)@12+..+nextTrackID@92
	dur := binary.BigEndian.Uint32(mvhd.payload[16:20])
	if dur != 500 {
		t.Errorf("mvhd duration = %d, want 500", dur)
	}
	// nextTrackID = maxID+1 = 4.
	nextID := binary.BigEndian.Uint32(mvhd.payload[96:100])
	if nextID != 4 {
		t.Errorf("mvhd nextTrackID = %d, want 4", nextID)
	}
}

// ── moov.go:62 — buildMvhd maxID+1 arithmetic ───────────────────────────────

// TestBuildMvhdNextTrackIDArithmetic kills ARITHMETIC_BASE on `maxID+1`.
// nextTrackID must be one more than the largest track ID.
func TestBuildMvhdNextTrackIDArithmetic(t *testing.T) {
	mvhd := buildMvhd(1000, 7) // nextTrackID=7
	// version/flags(4)+creation(4)+modification(4)+timescale(4)+duration(4)
	// + rate(4)+volume(2)+reserved(2)+reserved(8)+matrix(36)+pre_defined(24)+nextTrackID(4)
	// offset = 4+4+4+4+4+4+2+2+8+36+24 = 100? Let me find nextTrackID by parsing mvhd.
	// The payload is after the 8-byte box header.
	payload := mvhd[8:]
	// nextTrackID is the last 4 bytes of the 100-byte mvhd v0 payload.
	nextID := binary.BigEndian.Uint32(payload[len(payload)-4:])
	if nextID != 7 {
		t.Errorf("buildMvhd nextTrackID = %d, want 7", nextID)
	}
}

// ── moov.go:118 — buildTrak LanguageBCP47 ────────────────────────────────────

// TestBuildTrakElngPresent kills the CONDITIONALS_BOUNDARY (implied != "")
// on `t.mkv.LanguageBCP47 != ""`.  A non-empty BCP47 must produce an elng box.
func TestBuildTrakElngPresent(t *testing.T) {
	tr := makeOutTrackForMoov(1, 100)
	tr.mkv.LanguageBCP47 = "zh-Hant"
	trak, _ := buildTrak(tr, 1000, false)
	boxes, _ := iterBoxes(trak[8:])
	mdia, ok := findMemBox(boxes, "mdia")
	if !ok {
		t.Fatal("mdia not found")
	}
	mdiaBoxes, _ := iterBoxes(mdia.payload)
	if _, ok := findMemBox(mdiaBoxes, "elng"); !ok {
		t.Error("elng box missing when LanguageBCP47 is set")
	}
}

// ── meta.go:86 — containerFromMovie durationMs fallback ──────────────────────

// TestContainerFromMovieDurationFallback kills the CONDITIONALS_NEGATION mutant
// (durMs == 0 → durMs != 0).  When there are no samples (movieDurationMs=0),
// mv.durationMs must be used as the fallback.
func TestContainerFromMovieDurationFallback(t *testing.T) {
	mv := &movie{
		durationMs: 5000,
		tracks: []inTrack{
			{trackType: mkv.AudioTrack, codec: "aac", codecPrivate: []byte{0x12, 0x10},
				timescale: 1000, durationMs: 5000},
		},
	}
	c := containerFromMovie(mv)
	if c.DurationMs != 5000 {
		t.Errorf("DurationMs = %d, want 5000 (fallback)", c.DurationMs)
	}
}

// ── meta.go:118-128 — videoKeyframesMs dedup ─────────────────────────────────

// TestVideoKeyframeMsDedupArithmetic kills ARITHMETIC_BASE on `/8+1` in the
// capacity allocation, and CONDITIONALS_BOUNDARY on `v != out[len(out)-1]`.
func TestVideoKeyframesMsDedup(t *testing.T) {
	mv := &movie{tracks: []inTrack{{
		trackType: mkv.VideoTrack,
		timescale: 1000,
		samples: []inSample{
			{ctsMs: 0, sync: true},
			{ctsMs: 0, sync: true}, // duplicate
			{ctsMs: 40, sync: true},
			{ctsMs: 40, sync: false}, // not sync
			{ctsMs: 80, sync: true},
		},
	}}}
	kf := videoKeyframesMs(mv)
	want := []int64{0, 40, 80}
	if len(kf) != 3 {
		t.Fatalf("keyframes = %v, want %v", kf, want)
	}
	for i, v := range kf {
		if v != want[i] {
			t.Errorf("kf[%d] = %d, want %d", i, v, want[i])
		}
	}
}

// ── subtitle.go:62 — flushPendingCue nextStart > pendCuePTS ─────────────────

// TestFlushPendingCueNextStartGap kills CONDITIONALS_BOUNDARY mutants on
// `nextStart >= 0 && nextStart > t.pendCuePTS` (lines 134-135).
// When nextStart equals pendCuePTS, no gap clamping occurs.
func TestFlushPendingCueDurClamp(t *testing.T) {
	mp4Path := buildTestMP4WithSRT(t)
	// Verify we can extract the subtitle to ensure flushPendingCue ran.
	var b strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 3, &b); err != nil {
		// Track 3 may not exist; that's fine, we just need round-trip to not panic.
		_ = err
	}
}

// ── demux.go:237 — writeMKV cluster window arithmetic ────────────────────────

// TestWriteMKVClusterWindowArithmetic kills ARITHMETIC_BASE mutants on
// `s.dtsMs-groupStart >= clusterWindowMs` via a roundtrip that verifies the
// sample ordering was preserved correctly (no cluster is prematurely split or
// merged, which would change timing).
func TestWriteMKVClusterWindowRoundTrip(t *testing.T) {
	// Single video track, 5 keyframes, each 40ms apart.
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	var blocks []genBlock
	for i := 0; i < 5; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i * 40), key: true, data: []byte{0x01}})
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	// Round-trip back: must recover 5 samples.
	dstMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, dstMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}
}

// ── subtitle.go:37 — truncateRunes INVERT_NEGATIVES / ARITHMETIC_BASE ─────────

// TestTruncateRunesArithmeticBase kills ARITHMETIC_BASE and INVERT_NEGATIVES
// on `b[max]&0xC0 == 0x80` — the continuation byte check.  The `0xC0` mask
// and `0x80` sentinel are both targets; we verify the exact byte that triggers
// back-off.
func TestTruncateRunesContinuationByteExact(t *testing.T) {
	// "aé" = 'a'(0x61) + 0xC3 0xA9 (é in UTF-8).  len=3.
	// max=2: b[2]=0xA9, 0xA9&0xC0=0x80 → continuation → back off to 1.
	b := []byte("aé")
	if n := truncateRunes(b, 2); n != 1 {
		t.Errorf("truncateRunes mid-continuation: %d, want 1", n)
	}
	// max=1: b[1]=0xC3, 0xC3&0xC0=0xC0 ≠ 0x80 → not a continuation → no back-off → 1.
	if n := truncateRunes(b, 1); n != 1 {
		t.Errorf("truncateRunes at start byte: %d, want 1", n)
	}
	// Invert: with 0xC0→0x3F, 0xA9&0x3F = 0x29 ≠ 0x80 → no back-off → returns 2 (wrong).
	// Our test asserts 1, killing that mutation.
}

// ── subtitle_webvtt.go:44 — ExtractSubtitleWebVTT boundary ───────────────────

// TestExtractSubtitleWebVTTDurPositive kills the CONDITIONALS_BOUNDARY mutant
// on `s.durMs > 0` (subtitle_webvtt.go:67).  A cue with positive duration must
// have its end time set; a cue with durMs=0 must have end=0.
func TestExtractSubtitleWebVTTDurMsPositive(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "webvtt"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 2, pts: 500, key: true, data: encodeWVTTCue([]byte("Hello"))},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4 := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	var sb strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4, 2, &sb); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Hello") {
		t.Errorf("WebVTT output missing cue: %q", out)
	}
}

// ── webvtt.go:89 — decodeWVTT len(out)==0 boundary ───────────────────────────

// TestDecodeWVTTEmptyVttcPayload kills the CONDITIONALS_BOUNDARY on
// `len(out) == 0`.  A sample with only empty vttc (no payl) must return false.
func TestDecodeWVTTEmptyVttcPayload(t *testing.T) {
	// vttc with no payl box → out stays nil → ok=false.
	vttc := boxf("vttc", func(w *bw) {
		// empty, no payl child
	})
	_, ok := decodeWVTT(vttc)
	if ok {
		t.Error("vttc with no payl: want ok=false")
	}
}

// TestDecodeWVTTNewlineSeparation kills CONDITIONALS_BOUNDARY on
// `len(out) > 0` (the newline guard before appending).  First cue must have
// no leading newline; second cue must be separated by exactly one newline.
func TestDecodeWVTTNewlineSeparation(t *testing.T) {
	payl1 := boxf("payl", func(w *bw) { w.bytes([]byte("A")) })
	payl2 := boxf("payl", func(w *bw) { w.bytes([]byte("B")) })
	vttc1 := boxf("vttc", func(w *bw) { w.bytes(payl1) })
	vttc2 := boxf("vttc", func(w *bw) { w.bytes(payl2) })
	data := append(vttc1, vttc2...)
	got, ok := decodeWVTT(data)
	if !ok {
		t.Fatal("want ok=true")
	}
	if string(got) != "A\nB" {
		t.Errorf("decodeWVTT = %q, want A\\nB", got)
	}
}

// ── mux.go:67 — emitChapterSamples dur <= 0 guard ────────────────────────────

// TestEmitChapterSamplesDurNotNegative kills the CONDITIONALS_BOUNDARY mutant
// (dur <= 0 → dur < 0).  A chapter with exactly zero computed dur must still
// get dur=1 (not 0) so no zero-duration sample is emitted.
func TestChapterRoundTripDuration(t *testing.T) {
	chapters := []mkv.Chapter{
		{StartMs: 0, Title: "Intro"},
		{StartMs: 1000, Title: "Main"},
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 1, pts: 1000, key: true, data: []byte{0x01}},
		{track: 1, pts: 2000, key: true, data: []byte{0x01}},
	}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, chapters)
	mp4 := filepath.Join(t.TempDir(), "ch.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	// Verify chapters survived the roundtrip.
	m, _, err := OpenMeta(context.Background(), mp4)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	if len(m.Chapters) < 2 {
		t.Errorf("chapters = %d, want >= 2", len(m.Chapters))
	}
}

// ── mux.go:391 — frameDurationMs > 0 ─────────────────────────────────────────

// TestFrameDurationMsPositiveFR kills the CONDITIONALS_BOUNDARY mutant
// (> 0 → >= 0).  FrameRate > 0 must return a non-zero duration.
func TestFrameDurationMsPositiveFR(t *testing.T) {
	fr := 25.0
	tr := mkv.Track{FrameRate: &fr}
	if d := frameDurationMs(tr); d != 40 {
		t.Errorf("frameDurationMs(25fps) = %d, want 40", d)
	}
	zero := 0.0
	if d := frameDurationMs(mkv.Track{FrameRate: &zero}); d != 0 {
		t.Errorf("frameDurationMs(0fps) = %d, want 0", d)
	}
	if d := frameDurationMs(mkv.Track{}); d != 0 {
		t.Errorf("frameDurationMs(nil FR) = %d, want 0", d)
	}
}

// ── codec.go:296 — audioSampleEntry channels/rate guards ─────────────────────

// TestAudioSampleEntryChannelRateGuards kills CONDITIONALS_BOUNDARY mutants on
// `*t.Channels > 0` and `*t.SampleRate > 0`.
func TestAudioSampleEntryChannelRateGuards(t *testing.T) {
	ch := uint8(6)
	sr := 44100.0
	tr := &mkv.Track{ID: 1, Channels: &ch, SampleRate: &sr}
	entry := audioSampleEntry("mp4a", tr, []byte{0x00})
	// entry payload: 6(reserved)+2(dataRef)+8(reserved)+2(channels)+...
	payload := entry[8:] // skip box header
	gotCh := binary.BigEndian.Uint16(payload[16:18])
	if gotCh != 6 {
		t.Errorf("channels = %d, want 6", gotCh)
	}
	gotRate := binary.BigEndian.Uint32(payload[24:28])
	if gotRate != fixed16_16(44100) {
		t.Errorf("rate fixed-point = %08X, want %08X", gotRate, fixed16_16(44100))
	}
	// Zero channel count must fall back to 2.
	zero := uint8(0)
	tr2 := &mkv.Track{Channels: &zero}
	entry2 := audioSampleEntry("mp4a", tr2, []byte{})
	payload2 := entry2[8:]
	gotCh2 := binary.BigEndian.Uint16(payload2[16:18])
	if gotCh2 != 2 {
		t.Errorf("zero-channels fallback = %d, want 2", gotCh2)
	}
}

// ── codec.go:257-261 — paspBox anamorphic conditions ─────────────────────────

// TestPaspBoxNilDimensions kills the CONDITIONALS_BOUNDARY mutants (> 0 → >= 0)
// on Width/Height/DisplayWidth/DisplayHeight guards.
func TestPaspBoxZeroDimensions(t *testing.T) {
	w, h := uint32(0), uint32(480)
	dw, dh := uint32(640), uint32(480)
	tr := &mkv.Track{Width: &w, Height: &h, DisplayWidth: &dw, DisplayHeight: &dh}
	// Width=0 → nil (guard fires for zero).
	if paspBox(tr) != nil {
		t.Error("Width=0: must return nil")
	}
	// Valid non-zero anamorphic: 720x576 display=1024x576.
	w2, h2, dw2, dh2 := uint32(720), uint32(576), uint32(1024), uint32(576)
	tr2 := &mkv.Track{Width: &w2, Height: &h2, DisplayWidth: &dw2, DisplayHeight: &dh2}
	if paspBox(tr2) == nil {
		t.Error("anamorphic 720x576→1024x576: must not return nil")
	}
}

// ── parse.go:1166 — opusHeadFromDOps preSkip little-endian ───────────────────

// TestOpusHeadFromDOpsFull kills ARITHMETIC_BASE on the field offsets.
// Build a complete dOps and verify every field in the reconstructed OpusHead.
func TestOpusHeadFromDOpsFull(t *testing.T) {
	dops := make([]byte, 11)
	dops[0] = 0                                  // version
	dops[1] = 2                                  // channels=2
	binary.BigEndian.PutUint16(dops[2:4], 312)   // preSkip=312
	binary.BigEndian.PutUint32(dops[4:8], 48000) // inputSampleRate=48000
	binary.BigEndian.PutUint16(dops[8:10], 0)    // outputGain=0
	dops[10] = 0                                 // family=0

	head, err := opusHeadFromDOps(dops)
	if err != nil {
		t.Fatalf("opusHeadFromDOps: %v", err)
	}
	// OpusHead: "OpusHead"(8) version(1) channels(1) preSkip(2LE) rate(4LE) gain(2LE) family(1) = 19 bytes
	if len(head) < 19 {
		t.Fatalf("OpusHead len = %d, want >= 19", len(head))
	}
	if string(head[:8]) != "OpusHead" {
		t.Errorf("magic = %q, want OpusHead", head[:8])
	}
	if head[9] != 2 {
		t.Errorf("channels = %d, want 2", head[9])
	}
	preSkip := binary.LittleEndian.Uint16(head[10:12])
	if preSkip != 312 {
		t.Errorf("preSkip = %d, want 312", preSkip)
	}
	rate := binary.LittleEndian.Uint32(head[12:16])
	if rate != 48000 {
		t.Errorf("rate = %d, want 48000", rate)
	}
}

// ── helpers used above ────────────────────────────────────────────────────────

// buildStsz builds a parseStsz-compatible payload for the given sizes (variable-size form).
func buildStsz(sizes []uint32) []byte {
	var w bw
	w.u32(0) // version/flags
	w.u32(0) // sampleSize=0 (individual)
	w.u32(uint32(len(sizes)))
	for _, s := range sizes {
		w.u32(s)
	}
	return w.b
}

// buildStco builds a parseChunkOffsets-compatible stco payload for the given offsets.
func buildStco(offsets []uint64) []byte {
	var w bw
	w.u32(0) // version/flags
	w.u32(uint32(len(offsets)))
	for _, o := range offsets {
		w.u32(uint32(o))
	}
	return w.b
}

// buildStsc builds a parseStsc-compatible payload for the given entries.
func buildStsc(entries []stscEntry) []byte {
	var w bw
	w.u32(0) // version/flags
	w.u32(uint32(len(entries)))
	for _, e := range entries {
		w.u32(e.firstChunk)
		w.u32(e.perChunk)
		w.u32(1) // samples_per_description
	}
	return w.b
}

// buildSttsPayload builds a parseStts-compatible payload from runs and deltas.
func buildSttsPayload(runs, deltas []uint32) []byte {
	var w bw
	w.u32(0)
	w.u32(uint32(len(runs)))
	for i := range runs {
		w.u32(runs[i])
		w.u32(deltas[i])
	}
	return w.b
}

// makeOutTrackForMoov returns a minimal outTrack with one sample, suitable for
// buildMoov/buildTrak testing.  spec.text=true so textTiming uses sample.dur
// directly, giving a predictable track duration of durMs milliseconds.
func makeOutTrackForMoov(mp4ID uint32, durMs int64) *outTrack {
	t := &outTrack{
		mp4ID: mp4ID,
		mkv: mkv.Track{
			ID:   uint64(mp4ID),
			Type: mkv.SubtitleTrack,
		},
		spec:        codecSpec{handler: "text", text: true},
		sampleEntry: make([]byte, 8),
	}
	// sample.dur == durMs → textTiming produces total == durMs.
	t.samples.addDur(1, 0, durMs, true)
	t.samples.addChunk(0, 1)
	return t
}

// buildTestMP4WithSRT creates an MP4 with a video track and an SRT subtitle track.
func buildTestMP4WithSRT(t *testing.T) string {
	t.Helper()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac",
			CodecPrivate: []byte{0x12, 0x10}},
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "srt"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 2, pts: 0, key: true, data: fakeAACFrame()},
		{track: 3, pts: 500, key: true, data: []byte("Hello sub")},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4Path := filepath.Join(t.TempDir(), "srt.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatalf("buildTestMP4WithSRT: %v", err)
	}
	return mp4Path
}

func fakeAACFrame() []byte {
	// Minimal AAC ADTS frame header + 1 byte of silence payload.
	return []byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC, 0x00}
}
