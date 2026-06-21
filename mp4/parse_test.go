package mp4

import (
	"bytes"
	"testing"
)

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

func TestParseMdhdTimescale(t *testing.T) {
	// version 0: timescale at offset 12.
	var v0 bw
	v0.u32(0)     // version/flags
	v0.u32(0)     // creation
	v0.u32(0)     // modification
	v0.u32(48000) // timescale
	v0.u32(0)     // duration
	if got := parseMdhdTimescale(v0.b); got != 48000 {
		t.Errorf("v0 timescale = %d, want 48000", got)
	}
	// version 1: timescale at offset 20.
	var v1 bw
	v1.u8(1)
	v1.u24(0)     // flags
	v1.u64(0)     // creation
	v1.u64(0)     // modification
	v1.u32(90000) // timescale
	v1.u64(0)     // duration
	if got := parseMdhdTimescale(v1.b); got != 90000 {
		t.Errorf("v1 timescale = %d, want 90000", got)
	}
	if got := parseMdhdTimescale([]byte{0, 0}); got != 0 {
		t.Errorf("short mdhd = %d, want 0", got)
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
