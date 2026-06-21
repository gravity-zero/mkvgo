package mp4

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// makeAC3 builds a minimal AC-3 syncframe header with the given bsi fields.
func makeAC3(fscod, frmsizecod, bsid, bsmod, acmod, lfeon uint32) []byte {
	var w bitWriter
	w.write(0x0B77, 16)
	w.write(0, 16) // crc1
	w.write(fscod, 2)
	w.write(frmsizecod, 6)
	w.write(bsid, 5)
	w.write(bsmod, 3)
	w.write(acmod, 3)
	if acmod&0x1 != 0 && acmod != 0x1 {
		w.write(0, 2) // cmixlev
	}
	if acmod&0x4 != 0 {
		w.write(0, 2) // surmixlev
	}
	if acmod == 0x2 {
		w.write(0, 2) // dsurmod
	}
	w.write(lfeon, 1)
	w.write(0, 16) // trailing payload
	return w.bytes()
}

func readDac3(b []byte) (fscod, bsid, bsmod, acmod, lfeon, brc uint32) {
	r := &bitReader{data: b[8:]} // skip box header
	return r.bits(2), r.bits(5), r.bits(3), r.bits(3), r.bits(1), r.bits(5)
}

func TestParseAC3(t *testing.T) {
	// acmod=7 (3/2) exercises the cmixlev+surmixlev conditional skips.
	frame := makeAC3(0 /*48k*/, 20, 8, 1, 7, 1)
	dac3, err := parseAC3(frame)
	if err != nil {
		t.Fatal(err)
	}
	if string(dac3[4:8]) != "dac3" {
		t.Fatalf("not a dac3 box: %q", dac3[4:8])
	}
	fscod, bsid, bsmod, acmod, lfeon, brc := readDac3(dac3)
	if fscod != 0 || bsid != 8 || bsmod != 1 || acmod != 7 || lfeon != 1 {
		t.Errorf("dac3 fields = fscod%d bsid%d bsmod%d acmod%d lfeon%d", fscod, bsid, bsmod, acmod, lfeon)
	}
	if brc != 20>>1 {
		t.Errorf("bit_rate_code = %d, want %d", brc, 20>>1)
	}
}

func TestParseAC3SyncwordScan(t *testing.T) {
	frame := append([]byte{0xAA, 0xBB}, makeAC3(0, 20, 8, 0, 2, 0)...)
	if _, err := parseAC3(frame); err != nil {
		t.Errorf("should find syncword after a small offset: %v", err)
	}
	if _, err := parseAC3([]byte{0x00, 0x01, 0x02, 0x03}); err == nil {
		t.Error("expected error when no syncword present")
	}
}

func TestParseAC3RejectsEAC3Bsid(t *testing.T) {
	// bsid=16 is E-AC-3, not AC-3.
	frame := makeAC3(0, 20, 16, 0, 2, 0)
	if _, err := parseAC3(frame); err == nil {
		t.Error("expected AC-3 parser to reject bsid 16")
	}
}

// makeEAC3 builds a minimal E-AC-3 syncframe header.
func makeEAC3(frmsiz, fscod, numblkscod, acmod, lfeon, bsid uint32) []byte {
	var w bitWriter
	w.write(0x0B77, 16)
	w.write(0, 2) // strmtyp
	w.write(0, 3) // substreamid
	w.write(frmsiz, 11)
	w.write(fscod, 2)
	if fscod == 3 {
		w.write(0, 2) // fscod2
	} else {
		w.write(numblkscod, 2)
	}
	w.write(acmod, 3)
	w.write(lfeon, 1)
	w.write(bsid, 5)
	w.write(0, 16) // dialnorm etc + payload
	return w.bytes()
}

func TestParseEAC3(t *testing.T) {
	dec3, err := parseEAC3(makeEAC3(100, 0 /*48k*/, 3 /*6 blocks*/, 7, 1, 16))
	if err != nil {
		t.Fatal(err)
	}
	if string(dec3[4:8]) != "dec3" {
		t.Fatalf("not a dec3 box: %q", dec3[4:8])
	}
	r := &bitReader{data: dec3[8:]}
	dataRate := r.bits(13)
	numIndSub := r.bits(3)
	fscod := r.bits(2)
	bsid := r.bits(5)
	r.bits(1) // reserved
	r.bits(1) // asvc
	r.bits(3) // bsmod
	acmod := r.bits(3)
	lfeon := r.bits(1)
	if numIndSub != 0 {
		t.Errorf("num_ind_sub = %d, want 0", numIndSub)
	}
	if fscod != 0 || bsid != 16 || acmod != 7 || lfeon != 1 {
		t.Errorf("dec3 fields = fscod%d bsid%d acmod%d lfeon%d", fscod, bsid, acmod, lfeon)
	}
	if dataRate == 0 {
		t.Errorf("data_rate should be non-zero")
	}
}

func TestFlacEntryRoundTrip(t *testing.T) {
	// MKV FLAC CodecPrivate = "fLaC" marker + metadata blocks (here a stub block).
	meta := []byte{0x80, 0x00, 0x00, 0x22 /* STREAMINFO header */}
	meta = append(meta, bytes.Repeat([]byte{0x11}, 34)...)
	cp := append([]byte("fLaC"), meta...)
	tr := &mkv.Track{ID: 1, Codec: "flac", CodecPrivate: cp, Channels: u8p(2), SampleRate: f64p(44100)}

	entry, err := flacEntry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "fLaC" {
		t.Fatalf("entry type = %q, want fLaC", entry[4:8])
	}

	// Reverse: parse the sample entry back and check CodecPrivate is reconstructed.
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	var got inTrack
	ok, err := parseSampleEntry(&got, stsd[8:])
	if err != nil || !ok {
		t.Fatalf("parseSampleEntry: ok=%v err=%v", ok, err)
	}
	if got.codec != "flac" {
		t.Errorf("codec = %q, want flac", got.codec)
	}
	if !bytes.Equal(got.codecPrivate, cp) {
		t.Errorf("FLAC CodecPrivate round trip:\n got % x\nwant % x", got.codecPrivate, cp)
	}
}

func TestMP3Entry(t *testing.T) {
	tr := &mkv.Track{ID: 1, Codec: "A_MPEG/L3", Channels: u8p(2), SampleRate: f64p(44100)}
	entry, err := mp3Entry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "mp4a" {
		t.Fatalf("entry type = %q, want mp4a", entry[4:8])
	}
	// esds must declare objectTypeIndication 0x6B (MP3) with no DecoderSpecificInfo.
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	var got inTrack
	ok, err := parseSampleEntry(&got, stsd[8:])
	if err != nil || !ok {
		t.Fatalf("parseSampleEntry: ok=%v err=%v", ok, err)
	}
	if got.codec != "A_MPEG/L3" {
		t.Errorf("codec = %q, want A_MPEG/L3", got.codec)
	}
	if len(got.codecPrivate) != 0 {
		t.Errorf("MP3 must have no CodecPrivate, got % x", got.codecPrivate)
	}
}

func TestDTSEntry(t *testing.T) {
	tr := &mkv.Track{ID: 1, Codec: "dts", Channels: u8p(6), SampleRate: f64p(48000)}
	entry, err := dtsEntry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "mp4a" {
		t.Fatalf("entry type = %q, want mp4a", entry[4:8])
	}
	// Reverse: mp4a + esds objType 0xA9 must map back to DTS with no CodecPrivate.
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	var got inTrack
	ok, err := parseSampleEntry(&got, stsd[8:])
	if err != nil || !ok {
		t.Fatalf("parseSampleEntry: ok=%v err=%v", ok, err)
	}
	if got.codec != "dts" {
		t.Errorf("codec = %q, want dts", got.codec)
	}
	if len(got.codecPrivate) != 0 {
		t.Errorf("DTS must have no CodecPrivate, got % x", got.codecPrivate)
	}
}

// TestACEAC3ReverseMapping checks ac-3/ec-3 sample entries map back to the right
// MKV codec with no CodecPrivate.
func TestACEAC3ReverseMapping(t *testing.T) {
	cases := []struct {
		make  func() ([]byte, error)
		codec string
	}{
		{func() ([]byte, error) {
			return ac3Entry(&mkv.Track{ID: 1, Channels: u8p(2), SampleRate: f64p(48000)}, makeAC3(0, 20, 8, 0, 2, 0))
		}, "ac3"},
		{func() ([]byte, error) {
			return eac3Entry(&mkv.Track{ID: 1, Channels: u8p(6), SampleRate: f64p(48000)}, makeEAC3(100, 0, 3, 7, 1, 16))
		}, "eac3"},
	}
	for _, c := range cases {
		entry, err := c.make()
		if err != nil {
			t.Fatalf("%s build: %v", c.codec, err)
		}
		stsd := fullBox("stsd", 0, 0, func(w *bw) {
			w.u32(1)
			w.bytes(entry)
		})
		var got inTrack
		ok, err := parseSampleEntry(&got, stsd[8:])
		if err != nil || !ok {
			t.Fatalf("%s parse: ok=%v err=%v", c.codec, ok, err)
		}
		if got.codec != c.codec {
			t.Errorf("codec = %q, want %q", got.codec, c.codec)
		}
		if len(got.codecPrivate) != 0 {
			t.Errorf("%s must have no CodecPrivate", c.codec)
		}
	}
}
