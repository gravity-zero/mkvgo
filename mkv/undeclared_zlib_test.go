package mkv

import (
	"bytes"
	"compress/zlib"
	"testing"
)

// A subtitle block compressed with zlib but declared raw is inflated; a raw
// block that merely starts with 0x78, an audio block, and a stripped-header
// block are left to their own rules.
func TestDecodePayloadRecoversUndeclaredZlib(t *testing.T) {
	raw := []byte{0x16, 0x00, 0x0B, 0x07, 0x80, 0x04, 0x38, 0x10, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x17, 0x00}
	var zb bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zb, zlib.BestCompression)
	zw.Write(raw)
	zw.Close()
	sub := &Track{ID: 9, Type: SubtitleTrack, Codec: "pgs"}
	got, err := sub.DecodePayload(zb.Bytes())
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("undeclared zlib subtitle block: got %x err %v", got, err)
	}
	text := []byte("x^2 is not a zlib stream")
	if got, _ := sub.DecodePayload(text); !bytes.Equal(got, text) {
		t.Errorf("a raw block that only looks like zlib must stay raw: %q", got)
	}
	audio := &Track{ID: 2, Type: AudioTrack, Codec: "aac"}
	if got, _ := audio.DecodePayload(zb.Bytes()); !bytes.Equal(got, zb.Bytes()) {
		t.Errorf("an audio block is never sniffed")
	}
	if !looksZlib([]byte{0x78, 0xDA}) || !looksZlib([]byte{0x78, 0x9C}) || looksZlib([]byte{0x16, 0x00}) || looksZlib([]byte{0x78, 0x00}) {
		t.Errorf("looksZlib check is off")
	}
	// A stream whose checksum is wrong is not content: it stays as stored.
	bad := append([]byte(nil), zb.Bytes()...)
	bad[len(bad)-1] ^= 0xFF
	if got, rec, err := sub.DecodePayloadRecovered(bad); err != nil || rec || !bytes.Equal(got, bad) {
		t.Errorf("corrupted checksum: got %x recovered=%v err=%v", got, rec, err)
	}
	// The real-world false-positive shape: text that opens with 78 5E ("x^"), a valid zlib header pair.
	text78 := []byte("x^2 + y^2 = z^2")
	if (int(text78[0])<<8|int(text78[1]))%31 != 0 {
		t.Fatalf("fixture must pass the CMF/FLG check to be a false-positive candidate")
	}
	if got, rec, err := sub.DecodePayloadRecovered(text78); err != nil || rec || !bytes.Equal(got, text78) {
		t.Errorf("raw text starting with 78 5E: got %q recovered=%v err=%v", got, rec, err)
	}
}
