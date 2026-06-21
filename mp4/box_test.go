package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBox32BitFraming(t *testing.T) {
	b := box("test", []byte{0xAA, 0xBB})
	if len(b) != 10 {
		t.Fatalf("len = %d, want 10", len(b))
	}
	if got := binary.BigEndian.Uint32(b[0:4]); got != 10 {
		t.Errorf("size = %d, want 10", got)
	}
	if string(b[4:8]) != "test" {
		t.Errorf("type = %q, want test", b[4:8])
	}
	if !bytes.Equal(b[8:], []byte{0xAA, 0xBB}) {
		t.Errorf("payload = % x", b[8:])
	}
}

func TestMdatPlaceholderAndPatch(t *testing.T) {
	hdr := mdatHeaderPlaceholder()
	if len(hdr) != 16 {
		t.Fatalf("placeholder len = %d, want 16", len(hdr))
	}
	if binary.BigEndian.Uint32(hdr[0:4]) != 1 {
		t.Errorf("size field = %d, want 1 (largesize marker)", binary.BigEndian.Uint32(hdr[0:4]))
	}
	if string(hdr[4:8]) != "mdat" {
		t.Errorf("type = %q, want mdat", hdr[4:8])
	}
	if binary.BigEndian.Uint64(hdr[8:16]) != 0 {
		t.Errorf("largesize placeholder must be 0")
	}
}

func TestDescLen(t *testing.T) {
	cases := []struct {
		n    int
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7F}},
		{128, []byte{0x81, 0x00}},
		{0x3FFF, []byte{0xFF, 0x7F}},
		{0x4000, []byte{0x81, 0x80, 0x00}},
	}
	for _, c := range cases {
		var w bw
		w.descLen(c.n)
		if !bytes.Equal(w.b, c.want) {
			t.Errorf("descLen(%d) = % x, want % x", c.n, w.b, c.want)
		}
	}
}

func TestDescriptorRoundsLength(t *testing.T) {
	d := descriptor(0x03, []byte{0xDE, 0xAD})
	want := []byte{0x03, 0x02, 0xDE, 0xAD}
	if !bytes.Equal(d, want) {
		t.Errorf("descriptor = % x, want % x", d, want)
	}
}

func TestPackLanguage(t *testing.T) {
	// "und" = 0x55C4 per the ISO-639-2 packed encoding.
	if got := packLanguage("und"); got != 0x55C4 {
		t.Errorf("packLanguage(und) = %#04x, want 0x55C4", got)
	}
	// "eng"
	want := uint16('e'-0x60)<<10 | uint16('n'-0x60)<<5 | uint16('g'-0x60)
	if got := packLanguage("eng"); got != want {
		t.Errorf("packLanguage(eng) = %#04x, want %#04x", got, want)
	}
	// invalid → und
	if got := packLanguage("english"); got != 0x55C4 {
		t.Errorf("packLanguage(english) = %#04x, want und 0x55C4", got)
	}
	if got := packLanguage(""); got != 0x55C4 {
		t.Errorf("packLanguage(empty) = %#04x, want und", got)
	}
}

func TestFixed16_16(t *testing.T) {
	if got := fixed16_16(48000); got != 48000<<16 {
		t.Errorf("fixed16_16(48000) = %#x", got)
	}
}

func TestFullBoxHeader(t *testing.T) {
	b := fullBox("abcd", 1, 0x000123, func(w *bw) { w.u8(0xFF) })
	// size(4) type(4) version(1) flags(3) payload(1) = 13
	if len(b) != 13 {
		t.Fatalf("len = %d, want 13", len(b))
	}
	if b[8] != 1 {
		t.Errorf("version = %d, want 1", b[8])
	}
	if got := uint32(b[9])<<16 | uint32(b[10])<<8 | uint32(b[11]); got != 0x000123 {
		t.Errorf("flags = %#x, want 0x123", got)
	}
	if b[12] != 0xFF {
		t.Errorf("payload byte = %#x", b[12])
	}
}

func TestContainerConcatenatesChildren(t *testing.T) {
	c := container("moov", box("aaaa", nil), box("bbbb", nil))
	// moov size = 8 + (8 + 8) = 24
	if binary.BigEndian.Uint32(c[0:4]) != 24 {
		t.Fatalf("container size = %d, want 24", binary.BigEndian.Uint32(c[0:4]))
	}
	if string(c[12:16]) != "aaaa" || string(c[20:24]) != "bbbb" {
		t.Errorf("children not concatenated in order: % x", c)
	}
}
