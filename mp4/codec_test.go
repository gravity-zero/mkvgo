package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// makeOpusHead builds an OpusHead (RFC 7845) with the given mapping family.
func makeOpusHead(channels uint8, preSkip uint16, rate uint32, gain uint16, family uint8, mapping []byte) []byte {
	b := []byte("OpusHead")
	b = append(b, 1, channels)
	b = binary.LittleEndian.AppendUint16(b, preSkip)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint16(b, gain)
	b = append(b, family)
	b = append(b, mapping...)
	return b
}

func TestDOpsBoxByteExact(t *testing.T) {
	head := makeOpusHead(2, 312, 48000, 0, 0, nil)
	got, err := dOpsBox(head)
	if err != nil {
		t.Fatal(err)
	}
	// box header(8) + payload(11)
	want := []byte{
		0x00, 0x00, 0x00, 0x13, 'd', 'O', 'p', 's', // size=19, "dOps"
		0x00,       // version
		0x02,       // channel count
		0x01, 0x38, // preSkip = 312, big-endian
		0x00, 0x00, 0xBB, 0x80, // input sample rate = 48000, big-endian
		0x00, 0x00, // output gain
		0x00, // mapping family
	}
	if !bytes.Equal(got, want) {
		t.Errorf("dOps = % x\nwant   % x", got, want)
	}
}

func TestDOpsBoxWithChannelMapping(t *testing.T) {
	// family 1, 4 channels: streamCount, coupledCount, then 4 mapping bytes.
	mapping := []byte{0x02, 0x02, 0x00, 0x01, 0x02, 0x03}
	head := makeOpusHead(4, 0, 48000, 0, 1, mapping)
	got, err := dOpsBox(head)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, mapping) {
		t.Errorf("dOps must copy the channel mapping table verbatim: % x", got)
	}
}

func TestDOpsBoxRejectsBadHead(t *testing.T) {
	if _, err := dOpsBox([]byte("nope")); err == nil {
		t.Error("expected error for too-short head")
	}
	if _, err := dOpsBox(append([]byte("BADMAGIC"), make([]byte, 11)...)); err == nil {
		t.Error("expected error for wrong magic")
	}
	// family != 0 but mapping table missing
	truncated := makeOpusHead(2, 0, 48000, 0, 1, nil)
	if _, err := dOpsBox(truncated); err == nil {
		t.Error("expected error for truncated channel mapping")
	}
}

func TestEsdsBoxStructure(t *testing.T) {
	asc := []byte{0x12, 0x10} // AAC-LC 44100 stereo
	b := esdsBox(0x40, asc)
	if string(b[4:8]) != "esds" {
		t.Fatalf("not an esds box: %q", b[4:8])
	}
	// fullbox header is 8(box)+4(version/flags) = 12; ES_Descriptor tag follows.
	if b[12] != 0x03 {
		t.Fatalf("first descriptor tag = %#x, want 0x03 (ES_Descriptor)", b[12])
	}
	// The AudioSpecificConfig must appear verbatim somewhere in the box.
	if !bytes.Contains(b, asc) {
		t.Errorf("esds does not embed the AudioSpecificConfig")
	}
	// DecoderConfigDescriptor (0x04) and DecoderSpecificInfo (0x05) present.
	if !bytes.Contains(b, []byte{0x04}) || !bytes.Contains(b, []byte{0x05, byte(len(asc))}) {
		t.Errorf("esds missing decoder descriptors: % x", b)
	}
	// Object type indication 0x40 (AAC) present.
	if !bytes.Contains(b, []byte{0x40, 0x15}) {
		t.Errorf("esds missing AAC object type / audio stream type")
	}
}

func TestLookupCodec(t *testing.T) {
	for _, short := range []string{"h264", "hevc", "av1", "aac", "opus", "ac3", "eac3", "flac", "dts", "A_MPEG/L3", "srt"} {
		if _, ok := lookupCodec(short); !ok {
			t.Errorf("lookupCodec(%q) ok=false, want true", short)
		}
	}
	for _, short := range []string{"vp8", "vp9", "truehd", "pgs", ""} {
		if _, ok := lookupCodec(short); ok {
			t.Errorf("lookupCodec(%q) ok=true, want false", short)
		}
	}
}

func TestVisualEntryRequiresCodecPrivate(t *testing.T) {
	spec, _ := lookupCodec("h264")
	tr := &mkv.Track{ID: 1, Codec: "h264"} // no CodecPrivate
	if _, err := spec.sampleEntry(tr, nil); err == nil {
		t.Error("expected error when CodecPrivate is missing")
	}
	tr.CodecPrivate = []byte{0x01, 0x64, 0x00, 0x1F}
	w := uint32(1920)
	h := uint32(1080)
	tr.Width, tr.Height = &w, &h
	entry, err := spec.sampleEntry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "avc1" {
		t.Errorf("entry type = %q, want avc1", entry[4:8])
	}
	if !bytes.Contains(entry, append([]byte("avcC"), tr.CodecPrivate...)) {
		t.Errorf("avc1 entry must contain avcC + CodecPrivate")
	}
	// width/height are at a fixed offset in VisualSampleEntry.
	if !bytes.Contains(entry, []byte{0x07, 0x80, 0x04, 0x38}) { // 1920, 1080
		t.Errorf("entry missing width/height 1920x1080")
	}
}

func TestAudioEntryDefaults(t *testing.T) {
	spec, _ := lookupCodec("aac")
	tr := &mkv.Track{ID: 2, Codec: "aac", CodecPrivate: []byte{0x12, 0x10}}
	entry, err := spec.sampleEntry(tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "mp4a" {
		t.Errorf("entry type = %q, want mp4a", entry[4:8])
	}
}
