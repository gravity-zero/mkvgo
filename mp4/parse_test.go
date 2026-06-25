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
	_, dropped, err := parseTrak(craftTrak("vide", "jpeg", "", 2), 1<<20, 1000, sampleFull)
	if err != nil {
		t.Fatalf("parseTrak(cover): %v", err)
	}
	if dropped == nil || dropped.Type != mkv.VideoTrack || dropped.Codec != "jpeg" || dropped.ID != 2 {
		t.Errorf("cover drop = %+v, want {ID:2 Type:video Codec:jpeg}", dropped)
	}

	_, dropped, err = parseTrak(craftTrak("tmcd", "tmcd", "", 3), 1<<20, 1000, sampleFull)
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
	tr, _, err := parseTrak(craftTrak("vide", "jpeg", "forced-subtitle", 4), 1<<20, 1000, sampleFull)
	if err != nil {
		t.Fatalf("parseTrak(forced): %v", err)
	}
	if !tr.forcedKnown || !tr.forced {
		t.Errorf("forced kind: forced=%v known=%v, want true/true", tr.forced, tr.forcedKnown)
	}
	// A non-forced role is read (known) but must not mark the track forced.
	tr, _, err = parseTrak(craftTrak("vide", "jpeg", "subtitle", 5), 1<<20, 1000, sampleFull)
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
	sizes, err := parseStsz(w.b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != 100 || sizes[2] != 100 {
		t.Errorf("uniform stsz = %v, want [100 100 100]", sizes)
	}
	if _, err := parseStsz([]byte{0, 0, 0}, 0); err == nil {
		t.Error("expected error for short stsz")
	}
	// declared count with missing size array.
	var w2 bw
	w2.u32(0)
	w2.u32(0) // sample_size 0 → individual sizes follow
	w2.u32(5) // but none present
	if _, err := parseStsz(w2.b, 0); err == nil {
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
	objType, got, _, err := parseESDS(esdsBox(0x40, asc)[8:])
	if err != nil || objType != 0x40 || !bytes.Equal(got, asc) {
		t.Fatalf("AAC esds: obj=%#x asc=% x err=%v", objType, got, err)
	}
	// MP3: object type 0x6B, no DecoderSpecificInfo.
	objType, got, _, err = parseESDS(esdsBox(0x6B, nil)[8:])
	if err != nil || objType != 0x6B || len(got) != 0 {
		t.Fatalf("MP3 esds: obj=%#x asc=% x err=%v", objType, got, err)
	}
	if _, _, _, err := parseESDS([]byte{0x00}); err == nil {
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

// TestParseMP4Tags checks the udta/meta/ilst iTunes-tag reader: text atoms become
// Matroska SimpleTags (with the title surfaced separately), non-text atoms (cover
// art) are skipped.
func TestParseMP4Tags(t *testing.T) {
	data := func(s string) []byte { // a "data" box: type 1 (UTF-8), locale 0, value
		return box("data", append([]byte{0, 0, 0, 1, 0, 0, 0, 0}, s...))
	}
	cover := box("covr", box("data", []byte{0, 0, 0, 13, 0, 0, 0, 0, 0xFF, 0xD8})) // type 13 = JPEG, skipped
	ilst := box("ilst", bytes.Join([][]byte{
		box("\xa9nam", data("My Title")),
		box("\xa9too", data("Lavf60")),
		cover,
	}, nil))
	meta := box("meta", append([]byte{0, 0, 0, 0}, ilst...)) // meta FullBox: version/flags + children
	udtaBoxes, err := iterBoxes(meta)
	if err != nil {
		t.Fatal(err)
	}
	tags, title := parseMP4Tags(udtaBoxes)
	if title != "My Title" {
		t.Errorf("title = %q, want My Title", title)
	}
	got := map[string]string{}
	for _, st := range tags {
		got[st.Name] = st.Value
	}
	if got["TITLE"] != "My Title" || got["ENCODER"] != "Lavf60" {
		t.Errorf("tags = %v, want TITLE/ENCODER", got)
	}
	if _, ok := got["covr"]; ok || len(tags) != 2 {
		t.Errorf("cover art (non-text) must be skipped, got %d tags: %v", len(tags), got)
	}
}

// TestTkhdRotation checks the display-matrix → clockwise-rotation derivation for
// the four cardinal orientations (the matrix a,b entries, 16.16 fixed point).
func TestTkhdRotation(t *testing.T) {
	// buildTkhd builds a v0 tkhd with the given matrix a,b (rest of the matrix is
	// irrelevant to rotation); 40-byte prefix + matrix(36) + width/height(8).
	buildTkhd := func(a, b uint32) []byte {
		p := make([]byte, 40)
		p[0] = 0                   // version 0
		p = append(p, u32be(a)...) // matrix a
		p = append(p, u32be(b)...) // matrix b
		p = append(p, make([]byte, 28+8)...)
		return p
	}
	const one = 0x10000    // 1.0 in 16.16
	const neg = 0xFFFF0000 // -1.0 in 16.16
	for _, tt := range []struct {
		name string
		a, b uint32
		want int
	}{
		{"identity 0°", one, 0, 0},
		{"90° CW", 0, one, 90},
		{"180°", neg, 0, 180},
		{"270° CW", 0, neg, 270},
	} {
		if got := tkhdRotation(buildTkhd(tt.a, tt.b)); got != tt.want {
			t.Errorf("%s: rotation = %d, want %d", tt.name, got, tt.want)
		}
	}
	if got := tkhdRotation([]byte{0, 0, 0}); got != 0 {
		t.Errorf("short tkhd should be 0, got %d", got)
	}
}

// TestParseBitrate checks the btrt box → average bitrate, with the maxBitrate
// fallback when the average is zero.
func TestParseBitrate(t *testing.T) {
	btrt := func(maxBR, avgBR uint32) []byte {
		p := append(u32be(0), u32be(maxBR)...) // bufferSizeDB, maxBitrate
		return box("btrt", append(p, u32be(avgBR)...))
	}
	// avgBitrate present → used directly.
	entry := append(make([]byte, 28), btrt(256000, 192000)...)
	var tr inTrack
	parseBitrate(&tr, entry, 28)
	if tr.bitrate != 192000 {
		t.Errorf("avg bitrate = %d, want 192000", tr.bitrate)
	}
	// avgBitrate zero → fall back to maxBitrate.
	var tr2 inTrack
	parseBitrate(&tr2, append(make([]byte, 28), btrt(256000, 0)...), 28)
	if tr2.bitrate != 256000 {
		t.Errorf("max-fallback bitrate = %d, want 256000", tr2.bitrate)
	}
}

// TestParsePasp checks the pasp box → display aspect, stored exactly (no rounding
// that would collapse a fine ratio). Asserts via the ffprobe-equivalent helpers.
func TestParsePasp(t *testing.T) {
	asp := func(w, h, hs, vs uint32) (string, string) {
		entry := append(make([]byte, 78), box("pasp", append(u32be(hs), u32be(vs)...))...)
		tr := inTrack{width: w, height: h}
		parsePasp(&tr, entry, 78)
		mt := mkv.Track{Width: &w, Height: &h}
		if tr.displayWidth > 0 {
			dw, dh := tr.displayWidth, tr.displayHeight
			mt.DisplayWidth, mt.DisplayHeight = &dw, &dh
		}
		return mt.SampleAspectRatio(), mt.DisplayAspectRatio()
	}

	// 720×576, pasp 32:27 → SAR 32:27, DAR 40:27.
	if sar, dar := asp(720, 576, 32, 27); sar != "32:27" || dar != "40:27" {
		t.Errorf("32:27 pasp → sar=%s dar=%s, want 32:27 / 40:27", sar, dar)
	}
	// Real fine ratio (roubaix.mp4): 340×426, pasp 426:425 → SAR 426:425, DAR 4:5.
	// The old rounding collapsed this to 1:1; the exact ratio must survive.
	if sar, dar := asp(340, 426, 426, 425); sar != "426:425" || dar != "4:5" {
		t.Errorf("426:425 pasp → sar=%s dar=%s, want 426:425 / 4:5", sar, dar)
	}
	// Square pixels (1:1) leave the display dimensions unset → SAR 1:1, DAR coded.
	if sar, dar := asp(1920, 1080, 1, 1); sar != "1:1" || dar != "16:9" {
		t.Errorf("square pasp → sar=%s dar=%s, want 1:1 / 16:9", sar, dar)
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
