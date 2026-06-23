package mp4

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// craftTrak builds a minimal trak payload (tkhd + mdia, plus an optional
// udta/kind) with the given media handler and first sample-entry fourcc, for
// exercising the drop and forced-disposition paths. kindValue == "" omits udta.
func craftTrak(handler, sampleEntry, kindValue string, trackID uint32) []byte {
	mdhd := fullBox("mdhd", 0, 0, func(w *bw) {
		w.u32(0)    // creation
		w.u32(0)    // modification
		w.u32(1000) // timescale
		w.u32(0)    // duration
		w.u16(0)    // language
		w.u16(0)    // pre_defined
	})
	hdlr := fullBox("hdlr", 0, 0, func(w *bw) {
		w.u32(0) // pre_defined
		w.fourcc(handler)
		w.zeros(12)
		w.u8(0)
	})
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1) // entry_count
		w.bytes(box(sampleEntry, make([]byte, 80)))
	})
	mdia := container("mdia", mdhd, hdlr, container("minf", container("stbl", stsd)))
	tkhd := fullBox("tkhd", 0, 0x000007, func(w *bw) {
		w.u32(0) // creation
		w.u32(0) // modification
		w.u32(trackID)
		w.u32(0) // reserved
		w.u32(0) // duration
	})
	out := append(append([]byte{}, tkhd...), mdia...)
	if kindValue != "" {
		kind := fullBox("kind", 0, 0, func(w *bw) {
			w.bytes([]byte(dashRoleScheme))
			w.u8(0)
			w.bytes([]byte(kindValue))
			w.u8(0)
		})
		out = append(out, container("udta", kind)...)
	}
	return out
}

// TestParseTrakSurfacesDroppedTracks checks that a recognised-but-uncarried track
// is reported (not silently skipped): cover art (video handler, unsupported entry)
// and a non-media handler.
func TestParseTrakSurfacesDroppedTracks(t *testing.T) {
	_, dropped, err := parseTrak(craftTrak("vide", "jpeg", "", 2), 1<<20, 1000, true)
	if err != nil {
		t.Fatalf("parseTrak(cover): %v", err)
	}
	if dropped == nil || dropped.Type != mkv.VideoTrack || dropped.Codec != "jpeg" || dropped.ID != 2 {
		t.Errorf("cover drop = %+v, want {ID:2 Type:video Codec:jpeg}", dropped)
	}

	_, dropped, err = parseTrak(craftTrak("tmcd", "tmcd", "", 3), 1<<20, 1000, true)
	if err != nil {
		t.Fatalf("parseTrak(tmcd): %v", err)
	}
	if dropped == nil || dropped.Codec != "tmcd" {
		t.Errorf("non-media drop = %+v, want Codec:tmcd", dropped)
	}
}

func TestParseKind(t *testing.T) {
	kind := func(value string) []byte {
		return fullBox("kind", 0, 0, func(w *bw) {
			w.bytes([]byte(dashRoleScheme))
			w.u8(0)
			w.bytes([]byte(value))
			w.u8(0)
		})[8:] // strip the box header — parseKind reads the payload
	}
	if s, v := parseKind(kind("forced-subtitle")); s != dashRoleScheme || v != "forced-subtitle" {
		t.Errorf("parseKind = (%q, %q), want (%q, forced-subtitle)", s, v, dashRoleScheme)
	}
	if s, _ := parseKind([]byte{0, 0}); s != "" {
		t.Errorf("short kind scheme = %q, want empty", s)
	}
}

// TestParseTrakReadsForcedKind checks that a track-level DASH-role kind box sets
// the forced disposition — including on non-subtitle tracks (a real muxing quirk).
func TestParseTrakReadsForcedKind(t *testing.T) {
	tr, _, err := parseTrak(craftTrak("vide", "jpeg", "forced-subtitle", 4), 1<<20, 1000, true)
	if err != nil {
		t.Fatalf("parseTrak(forced): %v", err)
	}
	if !tr.forcedKnown || !tr.forced {
		t.Errorf("forced kind: forced=%v known=%v, want true/true", tr.forced, tr.forcedKnown)
	}
	// A non-forced role is read (known) but must not mark the track forced.
	tr, _, err = parseTrak(craftTrak("vide", "jpeg", "subtitle", 5), 1<<20, 1000, true)
	if err != nil {
		t.Fatalf("parseTrak(role): %v", err)
	}
	if tr.forced || !tr.forcedKnown {
		t.Errorf("subtitle role: forced=%v known=%v, want false/true", tr.forced, tr.forcedKnown)
	}
}

func TestParseStszUniformAndErrors(t *testing.T) {
	// sample_size != 0 → every sample shares that size, no size array.
	var w bw
	w.u32(0)   // version/flags
	w.u32(100) // sample_size
	w.u32(3)   // count
	sizes, err := parseStsz(w.b)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 100 || sizes[2] != 100 {
		t.Errorf("uniform stsz = %v, want [100 100 100]", sizes)
	}
	if _, err := parseStsz([]byte{0, 0, 0}); err == nil {
		t.Error("expected error for short stsz")
	}
	// declared count with missing size array.
	var w2 bw
	w2.u32(0)
	w2.u32(0) // sample_size 0 → individual sizes follow
	w2.u32(5) // but none present
	if _, err := parseStsz(w2.b); err == nil {
		t.Error("expected error for stsz declaring sizes it lacks")
	}
}

func TestParseChunkOffsetsStcoCo64(t *testing.T) {
	stco := func() bw {
		var w bw
		w.u32(0)
		w.u32(2)
		w.u32(100)
		w.u32(200)
		return w
	}()
	off, err := parseChunkOffsets([]memBox{{typ: "stco", payload: stco.b}})
	if err != nil || len(off) != 2 || off[0] != 100 || off[1] != 200 {
		t.Fatalf("stco parse = %v err=%v", off, err)
	}

	var c bw
	c.u32(0)
	c.u32(1)
	c.u64(0x1_0000_0000)
	off, err = parseChunkOffsets([]memBox{{typ: "co64", payload: c.b}})
	if err != nil || len(off) != 1 || off[0] != 0x1_0000_0000 {
		t.Fatalf("co64 parse = %v err=%v", off, err)
	}

	if _, err := parseChunkOffsets(nil); err == nil {
		t.Error("expected error when neither stco nor co64 present")
	}
	if _, err := parseChunkOffsets([]memBox{{typ: "stco", payload: []byte{0, 0, 0, 0, 0, 0, 0, 9}}}); err == nil {
		t.Error("expected error for stco shorter than declared")
	}
}

func TestParseMdhd(t *testing.T) {
	// version 0: timescale at offset 12, language at offset 20.
	var v0 bw
	v0.u32(0)                   // version/flags
	v0.u32(0)                   // creation
	v0.u32(0)                   // modification
	v0.u32(48000)               // timescale
	v0.u32(0)                   // duration
	v0.u16(packLanguage("fre")) // language
	v0.u16(0)                   // pre_defined
	if ts, lang := parseMdhd(v0.b); ts != 48000 || lang != "fre" {
		t.Errorf("v0 = (%d, %q), want (48000, \"fre\")", ts, lang)
	}
	// version 1: timescale at offset 20, language at offset 32.
	var v1 bw
	v1.u8(1)
	v1.u24(0)                   // flags
	v1.u64(0)                   // creation
	v1.u64(0)                   // modification
	v1.u32(90000)               // timescale
	v1.u64(0)                   // duration
	v1.u16(packLanguage("jpn")) // language
	v1.u16(0)                   // pre_defined
	if ts, lang := parseMdhd(v1.b); ts != 90000 || lang != "jpn" {
		t.Errorf("v1 = (%d, %q), want (90000, \"jpn\")", ts, lang)
	}
	if ts, lang := parseMdhd([]byte{0, 0}); ts != 0 || lang != "" {
		t.Errorf("short mdhd = (%d, %q), want (0, \"\")", ts, lang)
	}
}

func TestDecodeMdhdLanguage(t *testing.T) {
	cases := []struct {
		packed uint16
		want   string
	}{
		{packLanguage("fre"), "fre"},
		{packLanguage("und"), ""}, // undefined → treated as absent
		{0, "eng"},                // Macintosh language code 0 = English
		{1, "fra"},                // Macintosh language code 1 = French
		{0x2FF, ""},               // unknown Mac code (<0x400) → absent
		{0x7FFF, ""},              // unspecified packed value → rejected
	}
	for _, c := range cases {
		if got := decodeMdhdLanguage(c.packed); got != c.want {
			t.Errorf("decodeMdhdLanguage(%#04x) = %q, want %q", c.packed, got, c.want)
		}
	}
}

func TestTkhdEnabled(t *testing.T) {
	enabled := []byte{0, 0, 0, 0x07}  // enabled|in_movie|in_preview
	disabled := []byte{0, 0, 0, 0x06} // in_movie|in_preview, not enabled
	if !tkhdEnabled(enabled) {
		t.Error("flags 0x07 should be enabled")
	}
	if tkhdEnabled(disabled) {
		t.Error("flags 0x06 should not be enabled")
	}
	if tkhdEnabled([]byte{0, 0}) {
		t.Error("short tkhd should not be enabled")
	}
}

func TestParseElng(t *testing.T) {
	var b bw
	b.u32(0) // version/flags
	b.bytes([]byte("pt-BR"))
	b.u8(0) // null terminator
	if got := parseElng(b.b); got != "pt-BR" {
		t.Errorf("parseElng = %q, want pt-BR", got)
	}
	if got := parseElng([]byte{0, 0}); got != "" {
		t.Errorf("short elng = %q, want empty", got)
	}
}

func TestParseESDSAACAndMP3(t *testing.T) {
	asc := []byte{0x12, 0x10}
	objType, got, err := parseESDS(esdsBox(0x40, asc)[8:])
	if err != nil || objType != 0x40 || !bytes.Equal(got, asc) {
		t.Fatalf("AAC esds: obj=%#x asc=% x err=%v", objType, got, err)
	}
	// MP3: object type 0x6B, no DecoderSpecificInfo.
	objType, got, err = parseESDS(esdsBox(0x6B, nil)[8:])
	if err != nil || objType != 0x6B || len(got) != 0 {
		t.Fatalf("MP3 esds: obj=%#x asc=% x err=%v", objType, got, err)
	}
	if _, _, err := parseESDS([]byte{0x00}); err == nil {
		t.Error("expected error for short esds")
	}
}

func TestChildConfigErrors(t *testing.T) {
	if _, err := childConfig([]byte{1, 2}, 8, "avcC"); err == nil {
		t.Error("expected error when entry shorter than header")
	}
	// header present but no requested child box.
	entry := make([]byte, 28)
	entry = append(entry, box("xxxx", []byte{1, 2})...)
	if _, err := childConfig(entry, 28, "esds"); err == nil {
		t.Error("expected error when config box absent")
	}
	cfg := box("dOps", []byte{9, 9})
	got, err := childConfig(append(make([]byte, 28), cfg...), 28, "dOps")
	if err != nil || !bytes.Equal(got, []byte{9, 9}) {
		t.Fatalf("childConfig = % x err=%v", got, err)
	}
}

func TestTicksToMsAndDeref(t *testing.T) {
	if ticksToMs(1000, 0) != 1000 {
		t.Error("ticksToMs with zero timescale should return ticks unchanged")
	}
	if ticksToMs(90000, 90000) != 1000 {
		t.Errorf("ticksToMs(90000,90000) = %d, want 1000", ticksToMs(90000, 90000))
	}
	if derefU32(nil) != 0 {
		t.Error("derefU32(nil) should be 0")
	}
	v := uint32(7)
	if derefU32(&v) != 7 {
		t.Error("derefU32(&7) should be 7")
	}
}

// colrEntry builds a minimal visual sample entry payload (the 78-byte header
// followed by a colr box) carrying the given colour type and CICP code points.
func colrEntry(colrType string, primaries, transfer, matrix uint16, extra []byte) []byte {
	cb := make([]byte, 0, 10+len(extra))
	cb = append(cb, colrType...)
	cb = appendU16(cb, primaries)
	cb = appendU16(cb, transfer)
	cb = appendU16(cb, matrix)
	cb = append(cb, extra...)
	entry := make([]byte, 78)
	return append(entry, box("colr", cb)...)
}

func appendU16(b []byte, v uint16) []byte { return append(b, byte(v>>8), byte(v)) }

func u32be(v uint32) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }

// TestParsePasp checks the pasp box → display-dimension derivation: an anamorphic
// ratio sets DisplayWidth = Width·hSpacing/vSpacing; square pixels leave it unset.
func TestParsePasp(t *testing.T) {
	// pasp 32:27 over a 720×576 coded frame → display 720·32/27 = 853, height 576.
	entry := append(make([]byte, 78), box("pasp", append(u32be(32), u32be(27)...))...)
	tr := inTrack{width: 720, height: 576}
	parsePasp(&tr, entry, 78)
	if tr.displayWidth != 853 || tr.displayHeight != 576 {
		t.Errorf("anamorphic pasp → %dx%d, want 853x576", tr.displayWidth, tr.displayHeight)
	}

	// Square pixels (1:1) leave the display dimensions unset.
	square := append(make([]byte, 78), box("pasp", append(u32be(1), u32be(1)...))...)
	sq := inTrack{width: 1920, height: 1080}
	parsePasp(&sq, square, 78)
	if sq.displayWidth != 0 || sq.displayHeight != 0 {
		t.Errorf("square pasp should not set display dims, got %dx%d", sq.displayWidth, sq.displayHeight)
	}
}

// TestParseColr covers the colour-type handling: 'nclc' (no range byte) is read
// like 'nclx', and a stream specifying only the matrix (BT.709) with unspecified
// primaries/transfer still reports its colour_space — matching ffprobe.
func TestParseColr(t *testing.T) {
	deref := func(p *uint16) int {
		if p == nil {
			return -1
		}
		return int(*p)
	}

	// nclc with matrix=bt709 (1) but unspecified primaries/transfer (2).
	var nclc inTrack
	parseColr(&nclc, colrEntry("nclc", 2, 2, 1, nil), 78)
	if deref(nclc.colorMatrix) != 1 {
		t.Errorf("nclc matrix = %d, want 1 (bt709)", deref(nclc.colorMatrix))
	}
	if nclc.colorRange != nil {
		t.Errorf("nclc range = %d, want unset (no range byte)", deref(nclc.colorRange))
	}
	var mt mkv.Track
	mt.ColorSpace = nclc.colorMatrix
	if mt.ColorSpaceName() != "bt709" {
		t.Errorf("nclc ColorSpaceName = %q, want bt709", mt.ColorSpaceName())
	}

	// nclx full-range flag (high bit of the byte after the three code points).
	var nclx inTrack
	parseColr(&nclx, colrEntry("nclx", 9, 16, 9, []byte{0x80}), 78)
	if deref(nclx.colorMatrix) != 9 || deref(nclx.colorRange) != 2 {
		t.Errorf("nclx matrix/range = %d/%d, want 9/2", deref(nclx.colorMatrix), deref(nclx.colorRange))
	}

	// ICC-profile colr types are ignored.
	var prof inTrack
	parseColr(&prof, colrEntry("prof", 1, 1, 1, nil), 78)
	if prof.colorMatrix != nil {
		t.Errorf("prof colr should be ignored, got matrix %d", deref(prof.colorMatrix))
	}
}

func TestTruncateRunes(t *testing.T) {
	if n := truncateRunes([]byte("abc"), 10); n != 3 {
		t.Errorf("max>len → %d, want 3", n)
	}
	// "aé" = 0x61, 0xC3, 0xA9 — truncating at 2 lands mid-rune → back off to 1.
	if n := truncateRunes([]byte("aé"), 2); n != 1 {
		t.Errorf("mid-rune truncation = %d, want 1", n)
	}
	if n := truncateRunes([]byte("aé"), 3); n != 3 {
		t.Errorf("on-boundary truncation = %d, want 3", n)
	}
}
