package mp4

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// visualEntryBytes is a VisualSampleEntry of the type with its 78-byte header and one child box.
func visualEntryBytes(typ string, w, h uint16, child []byte) []byte {
	var b bw
	b.zeros(6)
	b.u16(1)    // data reference index
	b.zeros(16) // pre_defined, reserved
	b.u16(w)
	b.u16(h)
	b.u32(0x00480000)
	b.u32(0x00480000)
	b.u32(0)
	b.u16(1)
	b.zeros(32) // compressorname
	b.u16(24)
	b.i16(-1)
	b.bytes(child)
	return box(typ, b.b)
}

// audioEntryBytes is an AudioSampleEntry (version 0) of the type with children.
func audioEntryBytes(typ string, channels, bits uint16, rate uint32, children ...[]byte) []byte {
	var b bw
	b.zeros(6)
	b.u16(1)
	b.zeros(8) // version, revision, vendor
	b.u16(channels)
	b.u16(bits)
	b.zeros(4) // compression id, packet size
	b.u32(rate << 16)
	for _, c := range children {
		b.bytes(c)
	}
	return box(typ, b.b)
}

func parseOneEntry(t *testing.T, trackType mkv.TrackType, entry []byte) inTrack {
	t.Helper()
	stsd := fullBox("stsd", 0, 0, func(w *bw) {
		w.u32(1)
		w.bytes(entry)
	})
	tr := inTrack{trackType: trackType}
	ok, _, err := parseSampleEntry(&tr, stsd[8:])
	if err != nil || !ok {
		t.Fatalf("parseSampleEntry: ok=%v err=%v", ok, err)
	}
	return tr
}

// The sample entries Matroska has a form for and a remux carried as the mainstream muxer does: ALAC (A_ALAC, the
// 24-byte ALACSpecificConfig of the alac child, its version and flags left out), TrueHD (A_TRUEHD, no private
// data), MPEG-4 Part 2 (V_MPEG4/ISO/ASP, the esds DecoderSpecificInfo). from-mp4 refused all three by name.
func TestFromMP4CarriesALACTrueHDAndMPEG4Visual(t *testing.T) {
	cfg := bytes.Repeat([]byte{0xA5}, 24)
	alac := parseOneEntry(t, mkv.AudioTrack, audioEntryBytes("alac", 2, 16, 44100, fullBox("alac", 0, 0, func(w *bw) { w.bytes(cfg) })))
	if alac.codec != "A_ALAC" || !bytes.Equal(alac.codecPrivate, cfg) || alac.unsupported != "" || alac.channels != 2 || alac.sampleRate != 44100 {
		t.Errorf("alac: codec %q private %d bytes unsupported %q channels %d rate %v", alac.codec, len(alac.codecPrivate), alac.unsupported, alac.channels, alac.sampleRate)
	}
	short := parseOneEntry(t, mkv.AudioTrack, audioEntryBytes("alac", 2, 16, 44100, fullBox("alac", 0, 0, func(w *bw) { w.bytes(cfg[:10]) })))
	if short.unsupported == "" {
		t.Error("an alac child too short for the ALACSpecificConfig was carried")
	}
	truehd := parseOneEntry(t, mkv.AudioTrack, audioEntryBytes("mlpa", 2, 24, 48000, box("dmlp", make([]byte, 10))))
	if truehd.codec != "truehd" || len(truehd.codecPrivate) != 0 || truehd.unsupported != "" || truehd.channels != 2 {
		t.Errorf("mlpa: codec %q (want truehd, the reader's short name) private %d bytes unsupported %q channels %d", truehd.codec, len(truehd.codecPrivate), truehd.unsupported, truehd.channels)
	}
	dsi := []byte{0x00, 0x00, 0x01, 0xB0, 0xF5, 0x00, 0x00, 0x01, 0xB5, 0x09}
	mpeg4 := parseOneEntry(t, mkv.VideoTrack, visualEntryBytes("mp4v", 176, 144, esdsBox(0x20, dsi)))
	if mpeg4.codec != "V_MPEG4/ISO/ASP" || !bytes.Equal(mpeg4.codecPrivate, dsi) || mpeg4.unsupported != "" || mpeg4.width != 176 || mpeg4.height != 144 {
		t.Errorf("mp4v: codec %q private % x unsupported %q %dx%d", mpeg4.codec, mpeg4.codecPrivate, mpeg4.unsupported, mpeg4.width, mpeg4.height)
	}
	other := parseOneEntry(t, mkv.VideoTrack, visualEntryBytes("mp4v", 176, 144, esdsBox(0x6C, dsi)))
	if other.unsupported == "" {
		t.Error("an mp4v entry of another object type (JPEG) was carried as MPEG-4 Visual")
	}
}
