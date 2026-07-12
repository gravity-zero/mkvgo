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
	ok, _, err := parseSampleEntry(&got, stsd[8:])
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
	ok, _, err := parseSampleEntry(&got, stsd[8:])
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
	ok, _, err := parseSampleEntry(&got, stsd[8:])
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
		ok, _, err := parseSampleEntry(&got, stsd[8:])
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

// TestChannelCountsFromConfig verifies the read-side channel counts are taken
// from the codec configuration, not the (often wrong) AudioSampleEntry field.
func TestChannelCountsFromConfig(t *testing.T) {
	// AAC AudioSpecificConfig: AOT=2 (LC), sfi=4 (44100), channelConfiguration.
	asc := func(chanConfig uint32) []byte {
		var w bitWriter
		w.write(2, 5)
		w.write(4, 4)
		w.write(chanConfig, 4)
		return w.bytes()
	}
	if got := aacChannels(asc(2)); got != 2 {
		t.Errorf("aacChannels(stereo) = %d, want 2", got)
	}
	if got := aacChannels(asc(6)); got != 6 {
		t.Errorf("aacChannels(5.1) = %d, want 6", got)
	}
	if got := aacChannels(asc(7)); got != 8 {
		t.Errorf("aacChannels(7.1) = %d, want 8", got)
	}
	if got := aacChannels(fakeASC); got != 2 {
		t.Errorf("aacChannels(fakeASC) = %d, want 2", got)
	}

	// HE-AACv2 with explicit hierarchical signalling: AOT=29 (PS), mono core
	// (channelConfiguration=1) → the decoder upmixes to stereo, so 2 channels.
	explicitPS := func() []byte {
		var w bitWriter
		w.write(29, 5) // audioObjectType = PS
		w.write(4, 4)  // samplingFrequencyIndex
		w.write(1, 4)  // channelConfiguration = mono core
		w.write(4, 4)  // extensionSamplingFrequencyIndex
		w.write(2, 5)  // underlying audioObjectType = AAC-LC
		return w.bytes()
	}()
	if got := aacChannels(explicitPS); got != 2 {
		t.Errorf("aacChannels(explicit PS) = %d, want 2", got)
	}

	// HE-AACv2 with backward-compatible signalling: front says AAC-LC mono, a
	// trailing sync extension (0x2b7 SBR, then 0x548 PS) flags Parametric Stereo.
	backCompatPS := func(psFlag uint32) []byte {
		var w bitWriter
		w.write(2, 5) // audioObjectType = AAC-LC
		w.write(4, 4) // samplingFrequencyIndex
		w.write(1, 4) // channelConfiguration = mono
		// GASpecificConfig (AAC-LC, cc!=0): frameLengthFlag, dependsOnCoreCoder,
		// extensionFlag - all zero.
		w.write(0, 3)
		w.write(0x2b7, 11) // syncExtensionType: SBR
		w.write(5, 5)      // extension audioObjectType = SBR
		w.write(1, 1)      // sbrPresentFlag
		w.write(4, 4)      // extensionSamplingFrequencyIndex
		w.write(0x548, 11) // syncExtensionType: PS
		w.write(psFlag, 1) // psPresentFlag
		return w.bytes()
	}
	if got := aacChannels(backCompatPS(1)); got != 2 {
		t.Errorf("aacChannels(backward-compat PS) = %d, want 2", got)
	}
	// Same stream, SBR only (psPresentFlag=0): a mono core stays mono.
	if got := aacChannels(backCompatPS(0)); got != 1 {
		t.Errorf("aacChannels(SBR mono, no PS) = %d, want 1", got)
	}
	// Plain AAC-LC mono with no extension stays mono.
	monoLC := func() []byte {
		var w bitWriter
		w.write(2, 5)
		w.write(4, 4)
		w.write(1, 4)
		return w.bytes()
	}()
	if got := aacChannels(monoLC); got != 1 {
		t.Errorf("aacChannels(mono LC) = %d, want 1", got)
	}

	// AAC sample rate from the config: HE-AAC SBR codes a half-rate core and the
	// decoder doubles it; parseAACConfig reports both the core and the SBR output
	// rate so the caller can present the conventional output rate.
	// Explicit hierarchical SBR (AOT=5): core 22050, extension 44100, stereo.
	explicitSBR := func() []byte {
		var w bitWriter
		w.write(5, 5) // audioObjectType = SBR
		w.write(7, 4) // samplingFrequencyIndex = 22050 (core)
		w.write(2, 4) // channelConfiguration = stereo
		w.write(4, 4) // extensionSamplingFrequencyIndex = 44100 (output)
		w.write(2, 5) // underlying audioObjectType = AAC-LC
		return w.bytes()
	}()
	if cfg := parseAACConfig(explicitSBR); cfg.sampleRate != 22050 || cfg.outputRate != 44100 {
		t.Errorf("explicit SBR rates = %v/%v, want 22050/44100", cfg.sampleRate, cfg.outputRate)
	}

	// Backward-compatible SBR: front AAC-LC 24000, sync extension flags SBR at 48000.
	backCompatSBR := func() []byte {
		var w bitWriter
		w.write(2, 5)      // audioObjectType = AAC-LC
		w.write(6, 4)      // samplingFrequencyIndex = 24000 (core)
		w.write(2, 4)      // channelConfiguration = stereo
		w.write(0, 3)      // GASpecificConfig (cc!=0): all flags zero
		w.write(0x2b7, 11) // syncExtensionType: SBR
		w.write(5, 5)      // extension audioObjectType = SBR
		w.write(1, 1)      // sbrPresentFlag
		w.write(3, 4)      // extensionSamplingFrequencyIndex = 48000 (output)
		return w.bytes()
	}()
	if cfg := parseAACConfig(backCompatSBR); cfg.sampleRate != 24000 || cfg.outputRate != 48000 {
		t.Errorf("backward-compat SBR rates = %v/%v, want 24000/48000", cfg.sampleRate, cfg.outputRate)
	}

	// Plain AAC-LC: no SBR, so no output rate (the base rate stands).
	if cfg := parseAACConfig(fakeASC); cfg.sampleRate != 44100 || cfg.outputRate != 0 {
		t.Errorf("AAC-LC rates = %v/%v, want 44100/0", cfg.sampleRate, cfg.outputRate)
	}

	// Real-world explicit-SBR mono ASC (0x2b8a0800: AOT 5, core 22050, ext 44100,
	// channelConfiguration 1). When the stream carries in-band PS, probers report
	// 2 channels by decoding; head-only we report the ASC's mono core and the
	// doubled output rate. Documents the accepted in-band-PS limitation.
	inbandPS := []byte{0x2b, 0x8a, 0x08, 0x00}
	if cfg := parseAACConfig(inbandPS); cfg.channels != 1 || cfg.sampleRate != 22050 || cfg.outputRate != 44100 {
		t.Errorf("explicit-SBR mono ASC = %dch %v/%v, want 1ch 22050/44100",
			cfg.channels, cfg.sampleRate, cfg.outputRate)
	}

	// AC-3 dac3: acmod=7 (3/2), lfeon=1 → 5.1.
	dac3 := func() []byte {
		var w bitWriter
		w.write(0, 2) // fscod
		w.write(8, 5) // bsid
		w.write(0, 3) // bsmod
		w.write(7, 3) // acmod
		w.write(1, 1) // lfeon
		w.write(0, 5) // bit_rate_code
		w.write(0, 5) // reserved
		return w.bytes()
	}()
	if got := ac3Channels(dac3); got != 6 {
		t.Errorf("ac3Channels(5.1) = %d, want 6", got)
	}

	// E-AC-3 dec3: one independent substream, acmod=7, lfeon=1, no dep → 5.1.
	dec3 := func(numDepSub, chanLoc uint32) []byte {
		var w bitWriter
		w.write(0, 13) // data_rate
		w.write(0, 3)  // num_ind_sub - 1
		w.write(0, 2)  // fscod
		w.write(8, 5)  // bsid
		w.write(0, 1)  // reserved
		w.write(0, 1)  // asvc
		w.write(0, 3)  // bsmod
		w.write(7, 3)  // acmod
		w.write(1, 1)  // lfeon
		w.write(0, 3)  // reserved
		w.write(numDepSub, 4)
		if numDepSub > 0 {
			w.write(chanLoc, 9)
		} else {
			w.write(0, 1) // reserved
		}
		return w.bytes()
	}
	if got := eac3Channels(dec3(0, 0)); got != 6 {
		t.Errorf("eac3Channels(5.1) = %d, want 6", got)
	}
	// dependent substream carrying Lrs/Rrs (chan_loc bit 1 = 2 channels) → 7.1.
	if got := eac3Channels(dec3(1, 1<<1)); got != 8 {
		t.Errorf("eac3Channels(7.1) = %d, want 8", got)
	}
}
