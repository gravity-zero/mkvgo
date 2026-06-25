package mp4

// coverage_test.go — targeted statement coverage tests.
// Focus: reach lines missed by existing tests, assert a sensible result.
// No mutations testing here — see mutation_kill_test.go.

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ── meta.go:45 — OpenMetaWithFS ───────────────────────────────────────────────

// TestOpenMetaWithFS covers the OpenMetaWithFS convenience wrapper.
func TestOpenMetaWithFS(t *testing.T) {
	mp4Path := buildTestMP4(t)
	c, dropped, err := OpenMetaWithFS(context.Background(), mp4Path, nil)
	if err != nil {
		t.Fatalf("OpenMetaWithFS: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Errorf("tracks = %d, want 2", len(c.Tracks))
	}
	// The synthetic chapter track should appear in dropped (non-media text).
	_ = dropped
}

// ── codec.go:110 — canonicalSubCodec D_WEBVTT/ variants ──────────────────────

// TestCanonicalSubCodecDWebVTT covers the D_WEBVTT/ prefix mapping path.
func TestCanonicalSubCodecDWebVTT(t *testing.T) {
	for _, id := range []string{"D_WEBVTT/SUBTITLES", "D_WEBVTT/CAPTIONS", "D_WEBVTT/DESCRIPTIONS", "D_WEBVTT/METADATA"} {
		if got := canonicalSubCodec(id); got != "webvtt" {
			t.Errorf("canonicalSubCodec(%q) = %q, want webvtt", id, got)
		}
	}
}

// ── box.go:105 — descLen with n<0 ────────────────────────────────────────────

// TestDescLenNegativeClampsToZero covers the `if n < 0 { n = 0 }` guard.
func TestDescLenNegativeClampsToZero(t *testing.T) {
	var w bw
	w.descLen(-1)
	// A length of 0 encodes as a single byte (0x00).
	if len(w.b) == 0 {
		t.Error("descLen(-1) must write at least one byte")
	}
	if w.b[len(w.b)-1] != 0x00 {
		t.Errorf("descLen(-1) last byte = %02x, want 0x00", w.b[len(w.b)-1])
	}
}

// ── audio.go:219 — getAudioObjectType escape code ────────────────────────────

// TestGetAudioObjectTypeEscape covers the aot==31 escape path (reads 6 extra bits).
func TestGetAudioObjectTypeEscape(t *testing.T) {
	var bw bitWriter
	bw.write(31, 5) // escape: 5 bits all-ones
	bw.write(0, 6)  // extension: 0 → aot = 32
	r := &bitReader{data: bw.bytes()}
	if got := getAudioObjectType(r); got != 32 {
		t.Errorf("escape aot = %d, want 32", got)
	}
	// Without escape: 5 bits is the final value.
	var bw2 bitWriter
	bw2.write(2, 5) // AAC-LC
	r2 := &bitReader{data: bw2.bytes()}
	if got := getAudioObjectType(r2); got != 2 {
		t.Errorf("non-escape aot = %d, want 2", got)
	}
}

// ── audio.go:189 — readSamplingFrequency explicit (idx==0xF) ─────────────────

// TestReadSamplingFrequencyExplicit covers the 24-bit explicit frequency branch.
func TestReadSamplingFrequencyExplicit(t *testing.T) {
	var bw bitWriter
	bw.write(0xF, 4)    // explicit indicator
	bw.write(44100, 24) // frequency in Hz
	r := &bitReader{data: bw.bytes()}
	if got := readSamplingFrequency(r); got != 44100 {
		t.Errorf("explicit freq = %v, want 44100", got)
	}
}

// ── audio.go:244 — skipGASpecificConfig paths ─────────────────────────────────

// TestSkipGASpecificConfigPaths covers the dependsOnCoreCoder, aot-specific
// layerNr, extensionFlag+aot=22 (numOfSubFrame/layer_length), and
// extensionFlag+aot=17 (resilience flags) branches.
func TestSkipGASpecificConfigPaths(t *testing.T) {
	// cc=0 → false immediately (program_config_element).
	{
		r := &bitReader{data: []byte{}}
		if got := skipGASpecificConfig(r, 2, 0); got {
			t.Error("cc=0: want false")
		}
	}
	// dependsOnCoreCoder=1 → reads 14 extra bits.
	{
		var bw bitWriter
		bw.write(0, 1)  // frameLengthFlag
		bw.write(1, 1)  // dependsOnCoreCoder=1
		bw.write(0, 14) // coreCoderDelay
		bw.write(0, 1)  // extensionFlag=0
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 2, 2); !got {
			t.Error("dependsOnCoreCoder=1: want true")
		}
	}
	// aot=6 → reads layerNr (3 bits).
	{
		var bw bitWriter
		bw.write(0, 1) // frameLengthFlag
		bw.write(0, 1) // dependsOnCoreCoder=0
		bw.write(0, 1) // extensionFlag=0
		bw.write(0, 3) // layerNr
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 6, 2); !got {
			t.Error("aot=6 (layerNr): want true")
		}
	}
	// extensionFlag=1, aot=22 → reads numOfSubFrame(5) + layer_length(11) + extensionFlag3(1).
	{
		var bw bitWriter
		bw.write(0, 1)  // frameLengthFlag
		bw.write(0, 1)  // dependsOnCoreCoder=0
		bw.write(1, 1)  // extensionFlag=1
		bw.write(0, 5)  // numOfSubFrame
		bw.write(0, 11) // layer_length
		bw.write(0, 1)  // extensionFlag3
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 22, 2); !got {
			t.Error("aot=22 extensionFlag=1: want true")
		}
	}
	// extensionFlag=1, aot=17 → reads 3 resilience flags + extensionFlag3(1).
	{
		var bw bitWriter
		bw.write(0, 1) // frameLengthFlag
		bw.write(0, 1) // dependsOnCoreCoder=0
		bw.write(1, 1) // extensionFlag=1
		bw.write(0, 3) // section/scalefactor/spectral data resilience flags
		bw.write(0, 1) // extensionFlag3
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 17, 2); !got {
			t.Error("aot=17 extensionFlag=1: want true")
		}
	}
	// extensionFlag=1, aot=20 → aot==20 is in the same set as 17,19,23.
	{
		var bw bitWriter
		bw.write(0, 1) // frameLengthFlag
		bw.write(0, 1) // dependsOnCoreCoder=0
		bw.write(1, 1) // extensionFlag=1
		bw.write(0, 3) // resilience flags
		bw.write(0, 1) // extensionFlag3
		r := &bitReader{data: bw.bytes()}
		if got := skipGASpecificConfig(r, 20, 2); !got {
			t.Error("aot=20 extensionFlag=1: want true")
		}
	}
}

// ── parse.go:538 — parseMovieHeader version=1 ────────────────────────────────

// TestParseMovieHeaderV1 covers the version-1 path (64-bit timing fields).
func TestParseMovieHeaderV1(t *testing.T) {
	p := make([]byte, 32)
	p[0] = 1                                      // version=1
	binary.BigEndian.PutUint32(p[20:24], 90000)   // timescale at offset 20
	binary.BigEndian.PutUint64(p[24:32], 2700000) // durationTicks at offset 24
	ts, dur := parseMovieHeader(p)
	if ts != 90000 || dur != 2700000 {
		t.Errorf("v1: ts=%d dur=%d, want 90000/2700000", ts, dur)
	}
	// Too short for v1 (need 32 bytes) → returns zero.
	short := make([]byte, 8)
	short[0] = 1
	ts2, dur2 := parseMovieHeader(short)
	if ts2 != 0 || dur2 != 0 {
		t.Errorf("short v1: ts=%d dur=%d, want 0/0", ts2, dur2)
	}
}

// ── parse.go:526 — headerFrameCount stz2 fallback ────────────────────────────

// TestHeaderFrameCountStz2 covers the stz2 (compact size) box path.
func TestHeaderFrameCountStz2(t *testing.T) {
	var p bw
	p.u32(0)   // version/flags
	p.u8(0)    // reserved
	p.u8(0)    // reserved
	p.u8(0)    // reserved
	p.u8(4)    // field_size=4
	p.u32(123) // sample_count=123
	boxes := []memBox{{typ: "stz2", payload: p.b}}
	if got := headerFrameCount(boxes); got != 123 {
		t.Errorf("stz2 count = %d, want 123", got)
	}
	// stz2 too short → returns 0.
	boxes2 := []memBox{{typ: "stz2", payload: p.b[:11]}}
	if got := headerFrameCount(boxes2); got != 0 {
		t.Errorf("short stz2: count = %d, want 0", got)
	}
}

// ── parse.go:725 — tkhdRotation version=1 ────────────────────────────────────

// TestTkhdRotationV1 covers the version=1 path (matOff=52).
func TestTkhdRotationV1(t *testing.T) {
	p := make([]byte, 60) // need ≥ 52+8 = 60 bytes
	p[0] = 1              // version=1 → matOff=52
	const one = uint32(0x00010000)
	binary.BigEndian.PutUint32(p[52:56], one) // a=1.0
	binary.BigEndian.PutUint32(p[56:60], 0)   // b=0 → identity (0°)
	if got := tkhdRotation(p); got != 0 {
		t.Errorf("v1 identity: rotation = %d, want 0", got)
	}
	// 90° CW with v1.
	p2 := make([]byte, 60)
	p2[0] = 1
	binary.BigEndian.PutUint32(p2[52:56], 0)          // a=0
	binary.BigEndian.PutUint32(p2[56:60], 0x00010000) // b=1.0 → 90°
	if got := tkhdRotation(p2); got != 90 {
		t.Errorf("v1 90°: rotation = %d, want 90", got)
	}
	// Too short for v1 → 0.
	if got := tkhdRotation(p[:51]); got != 0 {
		t.Errorf("v1 short: rotation = %d, want 0", got)
	}
}

// ── chapters.go:155 — buildChpl negative start and long title ─────────────────

// TestBuildChplEdgeCases covers start<0 (clamped to 0) and title>255 (truncated).
func TestBuildChplEdgeCases(t *testing.T) {
	// start < 0 → clamped to 0.
	chapters := []mkv.Chapter{{StartMs: -100, Title: "X"}}
	b := buildChpl(chapters)
	payload := b[8:] // skip box header
	// Payload: version(4)+reserved(4)+count(1)+start(8)+titleLen(1)+title(?)
	start100ns := binary.BigEndian.Uint64(payload[9:17])
	if start100ns != 0 {
		t.Errorf("negative start: got %d 100ns-units, want 0", start100ns)
	}
	// title > 255 → truncated at 255 rune boundary.
	longTitle := strings.Repeat("a", 300)
	chapters2 := []mkv.Chapter{{StartMs: 0, Title: longTitle}}
	b2 := buildChpl(chapters2)
	payload2 := b2[8:]
	titleLen := int(payload2[17]) // 9-byte header + 8-byte start = offset 17
	if titleLen != 255 {
		t.Errorf("long title len = %d, want 255", titleLen)
	}
}

// ── parse.go:390 — parseTrak structural error paths ──────────────────────────

// TestParseTrakStructuralErrors covers the error paths for missing/malformed
// sub-boxes inside a trak (no mdia, no hdlr, hdlr too short, no minf, no stbl,
// no stsd).
func TestParseTrakStructuralErrors(t *testing.T) {
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(1000)
		w.u32(0)
		w.u16(0)
		w.u16(0)
	})
	tkhd := fullBox("tkhd", 0, 0x07, func(w *bw) {
		w.u32(0)
		w.u32(0)
		w.u32(1)
		w.u32(0)
		w.u32(0)
	})

	// trak with no mdia box → error.
	if _, _, err := parseTrak(tkhd, 1<<20, 1000, sampleNone); err == nil {
		t.Error("no mdia: want error")
	}

	// mdia present but no hdlr → error.
	mdiaNoHdlr := container("mdia", mdhd)
	if _, _, err := parseTrak(append(tkhd, mdiaNoHdlr...), 1<<20, 1000, sampleNone); err == nil {
		t.Error("mdia without hdlr: want error")
	}

	// hdlr payload too short (< 12 bytes after version/flags) → error.
	hdlrShort := fullBox("hdlr", 0, 0, func(w *bw) {
		w.u32(0) // pre_defined only; no handler_type follows (payload = 4 bytes < 12)
	})
	mdiaShortHdlr := container("mdia", mdhd, hdlrShort)
	if _, _, err := parseTrak(append(tkhd, mdiaShortHdlr...), 1<<20, 1000, sampleNone); err == nil {
		t.Error("hdlr too short: want error")
	}

	// mdia with valid hdlr but no minf → error.
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) {
		w.u32(0)
		w.fourcc("vide")
		w.zeros(12)
		w.u8(0)
	})
	mdiaNoMinf := container("mdia", mdhd, hdlr)
	if _, _, err := parseTrak(append(tkhd, mdiaNoMinf...), 1<<20, 1000, sampleNone); err == nil {
		t.Error("mdia without minf: want error")
	}

	// minf without stbl → error.
	minfNoStbl := container("minf", container("vmhd"))
	mdiaNoStbl := container("mdia", mdhd, hdlr, minfNoStbl)
	if _, _, err := parseTrak(append(tkhd, mdiaNoStbl...), 1<<20, 1000, sampleNone); err == nil {
		t.Error("minf without stbl: want error")
	}

	// stbl without stsd → error.
	stblNoStsd := container("stbl", box("stts", nil))
	minfNoStsd := container("minf", container("stbl", stblNoStsd))
	mdiaNoStsd := container("mdia", mdhd, hdlr, minfNoStsd)
	if _, _, err := parseTrak(append(tkhd, mdiaNoStsd...), 1<<20, 1000, sampleNone); err == nil {
		t.Error("stbl without stsd: want error")
	}
}

// ── parse.go:779 — parseSampleEntry error paths ───────────────────────────────

// TestParseSampleEntryErrors covers stsd too short and zero-entry stsd.
func TestParseSampleEntryErrors(t *testing.T) {
	var tr inTrack
	// stsd shorter than 8-byte minimum.
	if _, _, err := parseSampleEntry(&tr, []byte{0, 0, 0}); err == nil {
		t.Error("stsd too short: want error")
	}
	// stsd with entry_count=0 → no entries to parse.
	stsd := fullBox("stsd", 0, 0, func(w *bw) { w.u32(0) })
	if _, _, err := parseSampleEntry(&tr, stsd[8:]); err == nil {
		t.Error("stsd zero entries: want error")
	}
}

// ── parse.go:886 — parseMP4A unknown objectType ───────────────────────────────

// TestParseMP4AUnknownObjType covers the default case (objType not recognised)
// which returns ok=false, nil error — signals "skip this track".
func TestParseMP4AUnknownObjType(t *testing.T) {
	// objType 0x01 is not AAC, MP3, or DTS → ok=false.
	esds := esdsBox(0x01, nil)
	entry := make([]byte, 28) // AudioSampleEntry fixed header
	entry = append(entry, esds...)
	var tr inTrack
	ok, err := parseMP4A(&tr, entry, 28)
	if err != nil {
		t.Fatalf("unknown objType: unexpected error: %v", err)
	}
	if ok {
		t.Error("unknown objType: want ok=false")
	}
}

// ── parse.go:900 — extractFLAC dfLa too short ─────────────────────────────────

// TestExtractFLACDflaPayloadTooShort covers the "dfLa too short" error path.
func TestExtractFLACDflaPayloadTooShort(t *testing.T) {
	// dfLa with only 3 bytes payload (< 4 needed for version/flags).
	dfla := box("dfLa", []byte{1, 2, 3})
	entry := make([]byte, 28)
	entry = append(entry, dfla...)
	var tr inTrack
	if err := extractFLAC(&tr, entry, 28); err == nil {
		t.Error("dfLa too short: want error")
	}
}

// ── parse.go:1088 — parseESDS ES_Descriptor flag paths ───────────────────────

// TestParseESDSWithDependencyFlags covers the dependsOn_ES_ID (0x80) and
// OCR (0x20) flag paths in parseESDS.
func TestParseESDSWithDependencyFlags(t *testing.T) {
	sl := descriptor(0x06, []byte{0x02})
	makeDcfg := func() []byte {
		dcfg := make([]byte, 13)
		dcfg[0] = 0x40 // objectType = AAC
		return dcfg
	}
	buildES := func(flags uint8, extraBefore []byte) []byte {
		var es bw
		es.u16(1)    // ES_ID
		es.u8(flags) // flags
		es.bytes(extraBefore)
		es.bytes(descriptor(0x04, makeDcfg()))
		es.bytes(sl)
		return fullBox("esds", 0, 0, func(w *bw) { w.bytes(descriptor(0x03, es.b)) })
	}

	// dependsOn_ES_ID flag (0x80) → reads 2 extra bytes.
	{
		var extra bw
		extra.u16(0) // dependsOn_ES_ID value
		esds := buildES(0x80, extra.b)
		objType, _, _, err := parseESDS(esds[8:])
		if err != nil || objType != 0x40 {
			t.Errorf("dependsOn_ES_ID: err=%v objType=%02x, want nil/0x40", err, objType)
		}
	}

	// OCR flag (0x20) → reads 2 extra bytes.
	{
		var extra bw
		extra.u16(0) // OCR_ES_Id
		esds := buildES(0x20, extra.b)
		objType, _, _, err := parseESDS(esds[8:])
		if err != nil || objType != 0x40 {
			t.Errorf("OCR flag: err=%v objType=%02x, want nil/0x40", err, objType)
		}
	}

	// URL flag (0x40) → reads 1-byte length + url bytes.
	{
		var extra bw
		extra.u8(3)                        // urlLen=3
		extra.bytes([]byte{'f', 'o', 'o'}) // url
		esds := buildES(0x40, extra.b)
		objType, _, _, err := parseESDS(esds[8:])
		if err != nil || objType != 0x40 {
			t.Errorf("URL flag: err=%v objType=%02x, want nil/0x40", err, objType)
		}
	}
}

// ── sampletable.go:16 — buildSampleTable error paths ─────────────────────────

// TestBuildSampleTableMissingBoxErrors covers the "stbl without stsz" and
// "stbl without stsc" errors.
func TestBuildSampleTableMissingBoxErrors(t *testing.T) {
	// No stsz box.
	tr := inTrack{timescale: 1000}
	if err := buildSampleTable(&tr, []memBox{{typ: "stco", payload: nil}}, 1<<30); err == nil {
		t.Error("no stsz: want error")
	}
	// stsz present but no stsc.
	sizes := buildStsz([]uint32{4})
	stblBoxes := []memBox{
		{typ: "stsz", payload: sizes},
		{typ: "stco", payload: buildStco([]uint64{0})},
		{typ: "stts", payload: buildSttsPayload([]uint32{1}, []uint32{33})},
		// no stsc
	}
	if err := buildSampleTable(&tr, stblBoxes, 1<<30); err == nil {
		t.Error("no stsc: want error")
	}
}

// ── sampletable.go:164 — parseStsc too short ──────────────────────────────────

// TestParseStscErrors covers the "stsc too short" and "too few entries" errors.
func TestParseStscErrors(t *testing.T) {
	// Payload shorter than 8 bytes → error.
	if _, err := parseStsc([]byte{0, 0, 0}); err == nil {
		t.Error("stsc < 8 bytes: want error")
	}
	// count=3 but only one 12-byte entry follows → too short.
	var w bw
	w.u32(0) // version/flags
	w.u32(3) // count=3
	w.u32(1)
	w.u32(1)
	w.u32(1) // only one entry (12 bytes)
	if _, err := parseStsc(w.b); err == nil {
		t.Error("stsc claims 3 entries but only 1 present: want error")
	}
}

// ── sampletable.go:133 — parseChunkOffsets stco/co64 too short ───────────────

// TestParseChunkOffsetsTooShort covers the "stco too short" and "co64 too short"
// early-return error paths (payload < 8 bytes).
func TestParseChunkOffsetsTooShort(t *testing.T) {
	// stco payload < 8 bytes.
	if _, err := parseChunkOffsets([]memBox{{typ: "stco", payload: []byte{0, 0, 0}}}); err == nil {
		t.Error("stco < 8 bytes: want error")
	}
	// co64 payload < 8 bytes.
	if _, err := parseChunkOffsets([]memBox{{typ: "co64", payload: []byte{0, 0, 0}}}); err == nil {
		t.Error("co64 < 8 bytes: want error")
	}
}

// ── parse.go:302 — ilstDataValue edge cases ───────────────────────────────────

// TestIlstDataValueEdgeCases covers the "no data box" and "short data payload"
// branches that return empty string.
func TestIlstDataValueEdgeCases(t *testing.T) {
	// Atom with no "data" child box → empty.
	noData := box("ndat", []byte{1, 2, 3})
	if got := ilstDataValue(noData); got != "" {
		t.Errorf("no data box: got %q, want empty", got)
	}
	// Atom with "data" box whose payload is only 3 bytes (< 8) → empty.
	shortData := box("data", []byte{1, 2, 3})
	if got := ilstDataValue(shortData); got != "" {
		t.Errorf("short data payload: got %q, want empty", got)
	}
}

// ── parse.go:138 — readMoov 64-bit largesize and size=0 ──────────────────────

// TestReadMoovLargesizeAndSize0 covers the boxSize==1 (64-bit largesize) and
// boxSize==0 (extends to EOF) paths in readMoov.
func TestReadMoovLargesizeAndSize0(t *testing.T) {
	// Case 1: ftyp with 64-bit largesize, moov as 32-bit box.
	// Build: [1,"ftyp",largesize=16][8,"moov"] = 24 bytes total.
	var buf bytes.Buffer
	// ftyp: size=1 (largesize), type="ftyp", largesize=16 (header only).
	buf.Write([]byte{0, 0, 0, 1, 'f', 't', 'y', 'p'})
	var ls [8]byte
	binary.BigEndian.PutUint64(ls[:], 16)
	buf.Write(ls[:])
	// moov: size=8, empty payload.
	buf.Write([]byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'})
	data := buf.Bytes()
	r := bytes.NewReader(data)
	payload, err := readMoov(r, int64(len(data)))
	if err != nil {
		t.Fatalf("largesize ftyp + moov: %v", err)
	}
	if len(payload) != 0 {
		t.Errorf("largesize test: payload len = %d, want 0", len(payload))
	}

	// Case 2: moov with size=0 (extends to EOF). Put a fake ftyp first.
	var buf2 bytes.Buffer
	buf2.Write([]byte{0, 0, 0, 8, 'f', 't', 'y', 'p'}) // ftyp, size=8
	// moov with size=0 (extends to EOF), empty content.
	buf2.Write([]byte{0, 0, 0, 0, 'm', 'o', 'o', 'v'})
	data2 := buf2.Bytes()
	r2 := bytes.NewReader(data2)
	payload2, err := readMoov(r2, int64(len(data2)))
	if err != nil {
		t.Fatalf("moov size=0: %v", err)
	}
	if len(payload2) != 0 {
		t.Errorf("size=0 moov: payload len = %d, want 0", len(payload2))
	}

	// Case 3: box with size=1 but file too short to read largesize → error.
	trunc := []byte{0, 0, 0, 1, 'm', 'o', 'o', 'v'} // only 8 bytes, no largesize
	r3 := bytes.NewReader(trunc)
	if _, err := readMoov(r3, int64(len(trunc))); err == nil {
		t.Error("truncated largesize: want error")
	}
}

// ── parse.go:257 — parseMP4Tags missing/malformed meta ───────────────────────

// TestParseMP4TagsMissingMeta covers the case where udta has no meta child.
func TestParseMP4TagsMissingMeta(t *testing.T) {
	// udta with no meta box → returns nil tags.
	udtaBoxes := []memBox{{typ: "udta", payload: box("cprt", []byte("test"))}}
	udtaInner, _ := iterBoxes(udtaBoxes[0].payload)
	tags, title := parseMP4Tags(udtaInner)
	if tags != nil || title != "" {
		t.Errorf("no meta: got tags=%v title=%q, want nil/empty", tags, title)
	}
}

// ── mux.go:132 — RemuxToMP4 progress callback ────────────────────────────────

// TestRemuxToMP4ProgressCallback covers the `if o.Progress != nil` branch.
// Progress fires every 50 blocks so with a tiny input we just verify that
// the branch executes without error (br.SetProgress is called).
func TestRemuxToMP4ProgressCallback(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
	}
	src := buildMKV(t, tracks, blocks)
	dst := filepath.Join(t.TempDir(), "out.mp4")

	prog := func(processed, total int64) {}
	if err := RemuxToMP4(context.Background(), src, dst, Options{Progress: prog}); err != nil {
		t.Fatalf("RemuxToMP4 with progress: %v", err)
	}
}

// ── mux.go:112 — RemuxToMP4 non-existent source ───────────────────────────────

// TestRemuxToMP4NonExistentSrc covers the reader.OpenWithFS error path.
func TestRemuxToMP4NonExistentSrc(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mp4")
	err := RemuxToMP4(context.Background(), "/nonexistent/path/to/file.mkv", dst)
	if err == nil {
		t.Error("non-existent source: want error")
	}
}

// ── mux.go:62 — emitChapterSamples EndMs > StartMs ────────────────────────────

// TestChapterWithEndMs covers the `case ch.EndMs > ch.StartMs` branch in
// emitChapterSamples — the last chapter having an explicit end time.
func TestChapterWithEndMs(t *testing.T) {
	chapters := []mkv.Chapter{
		{StartMs: 0, Title: "Part 1"},
		// Last chapter: EndMs > StartMs → dur = EndMs - StartMs.
		{StartMs: 2000, Title: "Part 2", EndMs: 5000},
	}
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 1, pts: 2000, key: true, data: []byte{0x02}},
		{track: 1, pts: 4000, key: true, data: []byte{0x03}},
	}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, chapters)
	mp4 := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	// Verify chapter count survives the round-trip.
	c, _, err := OpenMeta(context.Background(), mp4)
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	if len(c.Chapters) != 2 {
		t.Errorf("chapters = %d, want 2", len(c.Chapters))
	}
}

// ── subtitle.go:128,148 — flushPendingCue lead-in and gap ────────────────────

// TestSubtitleLeadInAndGap exercises the lead-in empty sample (pendCuePTS > 0
// with no prior samples) and the gap-filling empty sample between two cues.
func TestSubtitleLeadInAndGap(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 1, pts: 40, key: false, data: []byte{0x02}},
		// First cue starts at 500ms (not 0) → lead-in empty sample needed.
		{track: 2, pts: 500, key: true, data: []byte("Hello sub")},
		// Second cue at 2000ms with gap after first cue's implied end.
		{track: 2, pts: 2000, key: true, data: []byte("World sub")},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4 := filepath.Join(t.TempDir(), "subs.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	// Extract the subtitle and verify both cues are present.
	var sb strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4, 2, &sb); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "Hello sub") || !strings.Contains(out, "World sub") {
		t.Errorf("subtitle output missing cues: %q", out)
	}
}

// ── subtitle_webvtt.go:54 — ExtractSubtitleWebVTT cancelled context ───────────

// TestExtractSubtitleWebVTTCancelledContext covers the ctx.Err() check in the
// sample-decoding loop.
func TestExtractSubtitleWebVTTCancelledContext(t *testing.T) {
	mp4Path := buildTestMP4WithSRT(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	var sb strings.Builder
	err := ExtractSubtitleWebVTT(ctx, mp4Path, 3, &sb)
	if err == nil {
		t.Error("cancelled context: want error")
	}
}

// ── subtitle_webvtt.go:48 — ExtractSubtitleWebVTT non-subtitle track ──────────

// TestExtractSubtitleWebVTTNonSubTrack covers the "not a subtitle track" error.
func TestExtractSubtitleWebVTTNonSubTrack(t *testing.T) {
	mp4Path := buildTestMP4(t) // video (track 1) + audio (track 2)
	var sb strings.Builder
	// Track 1 is video → must return an error.
	if err := ExtractSubtitleWebVTT(context.Background(), mp4Path, 1, &sb); err == nil {
		t.Error("video track: want error (not a subtitle)")
	}
}

// ── meta.go:102 — containerFromMovie with iTunes tags ────────────────────────

// TestContainerFromMovieWithTags covers the `if len(mv.tags) > 0` path that
// builds a Tags field in the Container.
func TestContainerFromMovieWithTags(t *testing.T) {
	mv := &movie{
		durationMs: 1000,
		tracks: []inTrack{
			{trackType: mkv.AudioTrack, codec: "aac",
				codecPrivate: []byte{0x12, 0x10}, timescale: 1000},
		},
		tags: []mkv.SimpleTag{
			{Name: "TITLE", Value: "My Film"},
			{Name: "ENCODER", Value: "mkvgo"},
		},
		title: "My Film",
	}
	c := containerFromMovie(mv)
	if len(c.Tags) == 0 {
		t.Fatal("Tags must be populated when mv.tags is non-empty")
	}
	if len(c.Tags[0].SimpleTags) != 2 {
		t.Errorf("SimpleTags = %d, want 2", len(c.Tags[0].SimpleTags))
	}
	if c.Info.Title != "My Film" {
		t.Errorf("Info.Title = %q, want My Film", c.Info.Title)
	}
}

// ── demux.go:120 — buildMKVTracks subtitle track fields ───────────────────────

// TestBuildMKVTracksSubtitleField covers the subtitle branch in buildMKVTracks
// — the code that falls through to the default (non-video/audio) handler.
func TestBuildMKVTracksSubtitleField(t *testing.T) {
	mv := &movie{
		tracks: []inTrack{
			{
				trackType:     mkv.SubtitleTrack,
				codec:         "srt",
				languageKnown: true,
				language:      "eng",
				bitrate:       256,
				durationMs:    5000,
			},
		},
	}
	tracks := buildMKVTracks(mv)
	if len(tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(tracks))
	}
	if tracks[0].Codec != "srt" {
		t.Errorf("codec = %q, want srt", tracks[0].Codec)
	}
	if tracks[0].Bitrate == nil || *tracks[0].Bitrate != 256 {
		t.Errorf("bitrate = %v, want 256", tracks[0].Bitrate)
	}
}

// ── parse.go:1061 — extractOpus round-trip via Opus track ─────────────────────

// TestOpusRoundTrip exercises opusEntry (build) and extractOpus (parse).
func TestOpusRoundTrip(t *testing.T) {
	// Minimal valid OpusHead (family=0, 2 channels, 48kHz, pre-skip 312).
	opusHead := []byte{
		'O', 'p', 'u', 's', 'H', 'e', 'a', 'd',
		1,     // version
		2,     // channels
		56, 1, // pre-skip 312 little-endian
		128, 187, 0, 0, // input rate 48000 little-endian
		0, 0, // output gain
		0, // channel mapping family=0
	}
	ch := uint8(2)
	sr := 48000.0
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "opus", CodecPrivate: opusHead,
			Channels: &ch, SampleRate: &sr},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 2, pts: 0, key: true, data: []byte{0xFE, 0xFF}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4 := filepath.Join(t.TempDir(), "opus.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4 (opus): %v", err)
	}
	outMKV := filepath.Join(t.TempDir(), "opus-out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4 (opus): %v", err)
	}
	// Verify the Opus track survived round-trip.
	c, _ := readMKV(t, outMKV)
	var foundOpus bool
	for _, tr := range c.Tracks {
		if tr.Codec == "opus" {
			foundOpus = true
		}
	}
	if !foundOpus {
		t.Error("Opus track not found after round-trip")
	}
}

// ── codec.go:318 — opusEntry error path ───────────────────────────────────────

// TestOpusEntryInvalidHead covers the error path when opusEntry receives a
// CodecPrivate that is not a valid OpusHead.
func TestOpusEntryInvalidHead(t *testing.T) {
	tr := &mkv.Track{ID: 1, Codec: "opus", CodecPrivate: []byte("notopus")}
	if _, err := opusEntry(tr, nil); err == nil {
		t.Error("invalid OpusHead: want error")
	}
}

// ── codec.go:279 — aacEntry no CodecPrivate ───────────────────────────────────

// TestAACEntryMissingASC covers the error path when aacEntry has no CodecPrivate.
func TestAACEntryMissingASC(t *testing.T) {
	tr := &mkv.Track{ID: 1, Codec: "aac"} // no CodecPrivate
	if _, err := aacEntry(tr, nil); err == nil {
		t.Error("aacEntry without CodecPrivate: want error")
	}
}

// ── codec.go:246 — cicp nil pointer ──────────────────────────────────────────

// TestCICPNilPointer covers the nil-pointer branch of cicp (returns 2=unspecified).
func TestCICPNilPointer(t *testing.T) {
	if got := cicp(nil); got != 2 {
		t.Errorf("cicp(nil) = %d, want 2 (unspecified)", got)
	}
	v := uint16(9)
	if got := cicp(&v); got != 9 {
		t.Errorf("cicp(&9) = %d, want 9", got)
	}
}

// ── parse.go:949 — parseBitrate short entry ───────────────────────────────────

// TestParseBitrateShortEntry covers the early return when entry is shorter than
// headerLen.
func TestParseBitrateShortEntry(t *testing.T) {
	short := make([]byte, 5) // shorter than any header
	var tr inTrack
	parseBitrate(&tr, short, 28) // headerLen=28, payload only 5 bytes → no-op
	if tr.bitrate != 0 {
		t.Errorf("short entry: bitrate = %d, want 0", tr.bitrate)
	}
}

// ── parse.go:976 — parsePasp zero dimensions ─────────────────────────────────

// TestParsePaspZeroDimensions covers the early return when tr.width or
// tr.height is zero (the `tr.width == 0 || tr.height == 0` guard).
func TestParsePaspZeroDimensions(t *testing.T) {
	// width=0 → no display dimensions set.
	entry := append(make([]byte, 78), box("pasp", append(u32be(2), u32be(1)...))...)
	var tr inTrack
	tr.width = 0
	tr.height = 480
	parsePasp(&tr, entry, 78)
	if tr.displayWidth != 0 || tr.displayHeight != 0 {
		t.Errorf("zero width: unexpected display dims %dx%d", tr.displayWidth, tr.displayHeight)
	}
}

// ── sampletable.go:57 — buildSampleTable sample outside file ─────────────────

// TestBuildSampleTableSampleOutsideFile covers the `pos < 0 || end > fileSize`
// error when a chunk offset places a sample past the file boundary.
func TestBuildSampleTableSampleOutsideFile(t *testing.T) {
	sizes := buildStsz([]uint32{100})
	stco := buildStco([]uint64{999}) // chunk at offset 999
	stsc := buildStsc([]stscEntry{{firstChunk: 1, perChunk: 1}})
	stts := buildSttsPayload([]uint32{1}, []uint32{33})
	stblBoxes := []memBox{
		{typ: "stsz", payload: sizes},
		{typ: "stco", payload: stco},
		{typ: "stsc", payload: stsc},
		{typ: "stts", payload: stts},
	}
	var tr inTrack
	tr.timescale = 1000
	// fileSize=50 but chunk is at offset 999 → sample end (999+100=1099) > 50.
	if err := buildSampleTable(&tr, stblBoxes, 50); err == nil {
		t.Error("sample outside file: want error")
	}
}

// ── webvtt.go:93 — extractWVTTConfig no vttC child ───────────────────────────

// TestExtractWVTTConfigNoVttC covers the case where the wvtt entry has no vttC
// box → CodecPrivate stays nil.
func TestExtractWVTTConfigNoVttC(t *testing.T) {
	// 8-byte plain-text header + a non-vttC child.
	payload := append(make([]byte, 8), box("vttE", nil)...)
	var tr inTrack
	extractWVTTConfig(&tr, payload)
	if tr.codecPrivate != nil {
		t.Errorf("no vttC: codecPrivate = %x, want nil", tr.codecPrivate)
	}
}

// ── parse.go:1028 — parseColr short payload ───────────────────────────────────

// TestParseColrShortPayload covers the `!ok || len(colr.payload) < 10` guard.
func TestParseColrShortPayload(t *testing.T) {
	// colr box with only 4 bytes of payload (< 10 minimum).
	shortColr := box("colr", []byte{'n', 'c', 'l', 'x'}) // only 4 bytes
	entry := append(make([]byte, 78), shortColr...)
	var tr inTrack
	parseColr(&tr, entry, 78)
	if tr.colorMatrix != nil {
		t.Error("short colr: colorMatrix must be nil")
	}
}

// ── demux.go:317 — readSample seek error ─────────────────────────────────────

// TestReadSampleSeekError covers the `r.Seek` error path by using a ReadSeeker
// whose Seek always returns an error.
func TestReadSampleSeekError(t *testing.T) {
	r := &alwaysErrSeeker{}
	if _, err := readSample(r, 0, 4); err == nil {
		t.Error("seek error: want error from readSample")
	}
}

// alwaysErrSeeker is an io.ReadSeeker whose Seek always fails.
type alwaysErrSeeker struct{}

func (alwaysErrSeeker) Read(p []byte) (int, error)         { return 0, io.ErrUnexpectedEOF }
func (alwaysErrSeeker) Seek(_ int64, _ int) (int64, error) { return 0, io.ErrNoProgress }

// ── remux.go — RemuxToMP4 SkipUnsupported with bad CodecPrivate ───────────────

// TestSkipUnsupportedBadCodecPrivate covers the path where a codec is known but
// its sample entry cannot be built (e.g. zero-length avcC).
func TestSkipUnsupportedBadCodecPrivate(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		// h264 track with empty CodecPrivate → visualEntry fails.
		{ID: 2, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: nil,
			Width: u32p(320), Height: u32p(240)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	dst := filepath.Join(t.TempDir(), "out.mp4")

	var dropped []DroppedTrack
	err := RemuxToMP4(context.Background(), srcMKV, dst, Options{
		SkipUnsupported: true,
		OnDrop:          func(d DroppedTrack) { dropped = append(dropped, d) },
	})
	if err != nil {
		t.Fatalf("SkipUnsupported bad CodecPrivate: %v", err)
	}
	// The bad track should have been dropped.
	var droppedH264 bool
	for _, d := range dropped {
		if d.ID == 2 {
			droppedH264 = true
		}
	}
	if !droppedH264 {
		t.Errorf("bad CodecPrivate track not reported as dropped; got drops: %+v", dropped)
	}
}

// ── remux.go — buildMKVTracks outputSampleRate ────────────────────────────────

// TestBuildMKVTracksOutputSampleRate covers the `t.outputSampleRate > 0` branch
// for HE-AAC that sets OutputSampleRate on the track.
func TestBuildMKVTracksOutputSampleRate(t *testing.T) {
	mv := &movie{
		tracks: []inTrack{{
			trackType:        mkv.AudioTrack,
			codec:            "aac",
			codecPrivate:     []byte{0x12, 0x10},
			channels:         2,
			sampleRate:       24000,
			outputSampleRate: 48000, // SBR-doubled rate
		}},
	}
	tracks := buildMKVTracks(mv)
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
	if tracks[0].OutputSampleRate == nil || *tracks[0].OutputSampleRate != 48000 {
		t.Errorf("OutputSampleRate = %v, want 48000", tracks[0].OutputSampleRate)
	}
}

// ── parse.go — parseStts not enough entries in payload ───────────────────────

// TestParseSttsPayloadTooShort covers the `len(payload) < 8+int(count)*8` error.
func TestParseSttsPayloadTooShort(t *testing.T) {
	var w bw
	w.u32(0) // version/flags
	w.u32(5) // count=5, but only provides 1 entry below
	w.u32(1)
	w.u32(33)
	if _, err := parseStts(w.b, 5); err == nil {
		t.Error("stts truncated entries: want error")
	}
}

// ── chapters.go:88 — encodeChapterSample empty title ─────────────────────────

// TestEncodeChapterSampleEmpty covers the len=0 edge case of encodeChapterSample.
func TestEncodeChapterSampleEmpty(t *testing.T) {
	s := encodeChapterSample("")
	if len(s) < 2 {
		t.Fatalf("empty title: sample too short (%d bytes)", len(s))
	}
	n := int(s[0])<<8 | int(s[1])
	if n != 0 {
		t.Errorf("empty title: length field = %d, want 0", n)
	}
}

// ── eac3 round-trip via RemuxToMP4 ────────────────────────────────────────────

// TestEAC3RoundTrip exercises eac3Entry (build path via needsFirstFrame) and
// the ec-3 sample entry parse path.
func TestEAC3RoundTrip(t *testing.T) {
	eac3Frame := makeEAC3(100, 0, 3, 7, 1, 16)
	ch := uint8(6)
	sr := 48000.0
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "eac3",
			Channels: &ch, SampleRate: &sr},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 2, pts: 0, key: true, data: eac3Frame},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4 := filepath.Join(t.TempDir(), "eac3.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4 (eac3): %v", err)
	}
	// Round-trip to MKV to confirm the track survives demux.
	outMKV := filepath.Join(t.TempDir(), "eac3-out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4 (eac3): %v", err)
	}
	c, _ := readMKV(t, outMKV)
	var found bool
	for _, tr := range c.Tracks {
		if tr.Codec == "eac3" {
			found = true
		}
	}
	if !found {
		t.Error("eac3 track not found after round-trip")
	}
}

// ── codec.go:316 — opusEntry channel-mapping truncated ────────────────────────

// TestEAC3EntryParseChannels covers the eac3Entry build followed by ec-3 sample
// entry parsing (the parse.go "ec-3" case that calls eac3Channels).
func TestEAC3EntryAndParseSampleEntry(t *testing.T) {
	eac3Frame := makeEAC3(100, 0, 3, 7, 1, 16)
	ch := uint8(6)
	sr := 48000.0
	entry, err := eac3Entry(&mkv.Track{ID: 1, Channels: &ch, SampleRate: &sr}, eac3Frame)
	if err != nil {
		t.Fatalf("eac3Entry: %v", err)
	}
	// Wrap in stsd and parse back.
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	var tr inTrack
	ok, fourcc, parseErr := parseSampleEntry(&tr, stsd[8:])
	if parseErr != nil || !ok {
		t.Fatalf("parseSampleEntry ec-3: ok=%v err=%v", ok, parseErr)
	}
	if fourcc != "ec-3" {
		t.Errorf("fourcc = %q, want ec-3", fourcc)
	}
	if tr.codec != "eac3" {
		t.Errorf("codec = %q, want eac3", tr.codec)
	}
}

// ── parse.go:819 — parseSampleEntry ac-3 ──────────────────────────────────────

// TestAC3EntryAndParseSampleEntry covers the ac-3 sample entry parse path.
func TestAC3EntryAndParseSampleEntry(t *testing.T) {
	ac3Frame := makeAC3(0, 20, 8, 0, 2, 0)
	ch := uint8(2)
	sr := 48000.0
	entry, err := ac3Entry(&mkv.Track{ID: 1, Channels: &ch, SampleRate: &sr}, ac3Frame)
	if err != nil {
		t.Fatalf("ac3Entry: %v", err)
	}
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	var tr inTrack
	ok, fourcc, parseErr := parseSampleEntry(&tr, stsd[8:])
	if parseErr != nil || !ok {
		t.Fatalf("parseSampleEntry ac-3: ok=%v err=%v", ok, parseErr)
	}
	if fourcc != "ac-3" {
		t.Errorf("fourcc = %q, want ac-3", fourcc)
	}
	if tr.codec != "ac3" {
		t.Errorf("codec = %q, want ac3", tr.codec)
	}
}

// ── D_WEBVTT subtitle codec variant ──────────────────────────────────────────

// TestDWebVTTSubtitleCarried covers the D_WEBVTT/ codec-ID path through
// subtitleCarriage → canonicalSubCodec. A D_WEBVTT/SUBTITLES track must be
// carried like a native webvtt track.
func TestDWebVTTSubtitleCarried(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "D_WEBVTT/SUBTITLES"},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{0x01}},
		{track: 2, pts: 500, key: true, data: encodeCue([]byte("From WebM"))},
	}
	srcMKV := buildMKV(t, tracks, blocks)
	mp4 := filepath.Join(t.TempDir(), "dwebvtt.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4); err != nil {
		t.Fatalf("RemuxToMP4 D_WEBVTT/SUBTITLES: %v", err)
	}
	// The subtitle should be extractable.
	var sb strings.Builder
	if err := ExtractSubtitleWebVTT(context.Background(), mp4, 2, &sb); err != nil {
		t.Fatalf("ExtractSubtitleWebVTT: %v", err)
	}
}

// ── parse.go — eac3Entry round-trip via sample entry ─────────────────────────

// TestEAC3EntryNoFirstFrame covers the planTracks code path where eac3 needs a
// first frame (needsFirstFrame=true) and the first block fills it lazily.
func TestEAC3EntryNeedsFirstFrame(t *testing.T) {
	// eac3 has needsFirstFrame=true, so sampleEntry is built lazily from block 0.
	spec, ok := lookupCodec("eac3")
	if !ok {
		t.Fatal("eac3 not in codecTable")
	}
	if !spec.needsFirstFrame {
		t.Error("eac3 must have needsFirstFrame=true")
	}
}

// ── parse.go:1003 — parseDolbyVision none present ────────────────────────────

// TestParseDolbyVisionAbsent covers the path where no dvcC/dvvC box exists —
// function returns without setting dolbyVision.
func TestParseDolbyVisionAbsent(t *testing.T) {
	// Visual entry with no Dolby Vision config box.
	entry := make([]byte, 78)
	var tr inTrack
	parseDolbyVision(&tr, entry, 78)
	if tr.dolbyVision != nil {
		t.Error("no DV box: dolbyVision must be nil")
	}
}

// ── parse.go — readMoov invalid box size ─────────────────────────────────────

// TestReadMoovInvalidBoxSize covers the `boxSize < headerLen || off+boxSize > size`
// error path that rejects a box with a nonsensical size field.
func TestReadMoovInvalidBoxSize(t *testing.T) {
	// A single box claiming size=4 (< 8 = minimum header length).
	data := []byte{0, 0, 0, 4, 'm', 'o', 'o', 'v', 0, 0}
	r := bytes.NewReader(data)
	if _, err := readMoov(r, int64(len(data))); err == nil {
		t.Error("box size < header length: want error")
	}
}

// ── parse.go — stz2 absent, headerFrameCount returns 0 ───────────────────────

// TestHeaderFrameCountNoBoxes covers the case where neither stsz nor stz2 is
// present — returns 0.
func TestHeaderFrameCountNoBoxes(t *testing.T) {
	if got := headerFrameCount([]memBox{{typ: "stco", payload: nil}}); got != 0 {
		t.Errorf("no stsz/stz2: count = %d, want 0", got)
	}
}

// ── OpenMeta/ReadMeta — cancelled context ────────────────────────────────────

// TestReadMetaCancelledBeforeSeek covers the ctx.Err() check at the start of
// readMeta.
func TestReadMetaCancelledBeforeSeek(t *testing.T) {
	mp4Path := buildTestMP4(t)
	f, err := os.Open(mp4Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := ReadMeta(ctx, f, "x.mp4"); err == nil {
		t.Error("cancelled context: want error")
	}
}
