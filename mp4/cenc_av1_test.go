package mp4

// cenc_av1_test.go - round-trip and invariant coverage for splitAV1Subsamples
// (cenc_av1.go): builds synthetic AV1 temporal units (OBU sequences with
// explicit leb128 size fields) and checks the clear/protected split against
// the rules documented in cenc_av1.go.

import (
	"bytes"
	"crypto/aes"
	"testing"
)

// obuLenPrefixed builds one low-overhead-format OBU: header byte (no
// extension), leb128 obu_size, then payload. size fits a single leb128 byte
// (< 128) for every fixture in this file.
func obuLenPrefixed(obuType byte, payload []byte) []byte {
	if len(payload) >= 128 {
		panic("cenc_av1_test: fixture payload too large for a single-byte leb128")
	}
	header := obuType<<3 | 0x02 // obu_extension_flag=0, obu_has_size_field=1, reserved=0
	b := []byte{header, byte(len(payload))}
	return append(b, payload...)
}

// av1TemporalDelimiter, av1SequenceHeader, av1TileGroup and av1FrameOBU build
// the fixture OBUs used below.
func av1TemporalDelimiter() []byte { return obuLenPrefixed(obuTemporalDelimiter, nil) }

func av1SequenceHeader() []byte {
	return obuLenPrefixed(obuSeqHeader, []byte{0xAA, 0xBB, 0xCC, 0xDD})
}

func av1TileGroup(payloadLen int) []byte {
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	return obuLenPrefixed(obuTileGroup, payload)
}

func av1FrameOBU(payloadLen int) []byte {
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte(0x80 + i)
	}
	return obuLenPrefixed(obuFrame, payload)
}

// av1TemporalUnit concatenates a temporal_delimiter, a sequence_header and a
// caller-supplied trailing OBU into one sample.
func av1TemporalUnit(trailing []byte) []byte {
	var sample []byte
	sample = append(sample, av1TemporalDelimiter()...)
	sample = append(sample, av1SequenceHeader()...)
	sample = append(sample, trailing...)
	return sample
}

func TestAV1CENCRoundTrip(t *testing.T) {
	tg := av1TileGroup(50) // 50 % 16 = 2 -> exercises the trailing-remainder rule
	sample := av1TemporalUnit(tg)

	subs, err := splitAV1Subsamples(sample)
	if err != nil {
		t.Fatalf("splitAV1Subsamples: %v", err)
	}

	var total int
	for _, s := range subs {
		total += int(s.clear) + int(s.protected)
	}
	if total != len(sample) {
		t.Fatalf("subsample sizes sum to %d, want %d (len(sample))", total, len(sample))
	}

	key := []byte("0123456789abcdef")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	counterBlock := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	original := append([]byte(nil), sample...)
	working := append([]byte(nil), sample...)

	cencCTREncrypt(block, counterBlock, working, subs)
	if bytes.Equal(working, original) {
		t.Fatalf("encryption did not change any byte of the sample (protected region must be non-empty)")
	}

	// CTR is symmetric: encrypting the ciphertext again with the same
	// counter recovers the plaintext.
	cencCTREncrypt(block, counterBlock, working, subs)
	if !bytes.Equal(working, original) {
		t.Fatalf("round-trip mismatch:\n got  %x\n want %x", working, original)
	}
}

func TestAV1CENCInvariants(t *testing.T) {
	tg := av1TileGroup(50)
	sample := av1TemporalUnit(tg)
	tdLen := len(av1TemporalDelimiter())
	seqLen := len(av1SequenceHeader())

	subs, err := splitAV1Subsamples(sample)
	if err != nil {
		t.Fatalf("splitAV1Subsamples: %v", err)
	}

	key := []byte("0123456789abcdef")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	counterBlock := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	original := append([]byte(nil), sample...)
	working := append([]byte(nil), sample...)
	cencCTREncrypt(block, counterBlock, working, subs)

	// temporal_delimiter and sequence_header (OBU headers, leb128 sizes and
	// payloads alike) must be entirely untouched by encryption.
	prefixLen := tdLen + seqLen
	if !bytes.Equal(working[:prefixLen], original[:prefixLen]) {
		t.Fatalf("temporal_delimiter/sequence_header bytes changed after encryption:\n got  %x\n want %x",
			working[:prefixLen], original[:prefixLen])
	}

	// The tile_group OBU's header (obu_header + leb128 obu_size) must also
	// be unchanged.
	tgHeaderLen := 2 // obu_header(1) + single-byte leb128 size(1), per obuLenPrefixed
	tgHeaderStart := prefixLen
	tgHeaderEnd := tgHeaderStart + tgHeaderLen
	if !bytes.Equal(working[tgHeaderStart:tgHeaderEnd], original[tgHeaderStart:tgHeaderEnd]) {
		t.Fatalf("tile_group OBU header/size changed after encryption:\n got  %x\n want %x",
			working[tgHeaderStart:tgHeaderEnd], original[tgHeaderStart:tgHeaderEnd])
	}

	// The tile_group payload is 50 bytes: 48 protected (a multiple of 16)
	// plus a 2-byte clear remainder at the end.
	payloadStart := tgHeaderEnd
	protectedLen := 48
	if bytes.Equal(working[payloadStart:payloadStart+protectedLen], original[payloadStart:payloadStart+protectedLen]) {
		t.Fatalf("tile_group payload's protected region was not encrypted")
	}
	remainderStart := payloadStart + protectedLen
	if !bytes.Equal(working[remainderStart:], original[remainderStart:]) {
		t.Fatalf("tile_group payload's trailing < 16-byte remainder was encrypted (must stay clear):\n got  %x\n want %x",
			working[remainderStart:], original[remainderStart:])
	}

	// Every protected region reported by the split must be a multiple of 16.
	for _, s := range subs {
		if s.protected%16 != 0 {
			t.Fatalf("protected subsample length %d is not a multiple of 16", s.protected)
		}
	}
}

func TestAV1CENCNonTileOBUsFullyClear(t *testing.T) {
	sample := av1TemporalUnit(nil)
	subs, err := splitAV1Subsamples(sample)
	if err != nil {
		t.Fatalf("splitAV1Subsamples: %v", err)
	}
	for _, s := range subs {
		if s.protected != 0 {
			t.Fatalf("temporal_delimiter + sequence_header sample must be entirely clear, got protected=%d", s.protected)
		}
	}
	var total int
	for _, s := range subs {
		total += int(s.clear) + int(s.protected)
	}
	if total != len(sample) {
		t.Fatalf("subsample sizes sum to %d, want %d", total, len(sample))
	}
}

// TestAV1CENCFrameOBUConservativelyClear documents and locks in the
// deliberate conservative choice for OBU_FRAME: since this package cannot
// parse the AV1 uncompressed frame header, it leaves the entire OBU_FRAME
// payload clear rather than guess the tile-data boundary (see cenc_av1.go's
// doc comment). Content requiring encryption of coded frame data must be
// produced with OBU_FRAME_HEADER + OBU_TILE_GROUP instead of a combined
// OBU_FRAME.
func TestAV1CENCFrameOBUConservativelyClear(t *testing.T) {
	frame := av1FrameOBU(40)
	sample := av1TemporalUnit(frame)

	subs, err := splitAV1Subsamples(sample)
	if err != nil {
		t.Fatalf("splitAV1Subsamples: %v", err)
	}

	var total, protected int
	for _, s := range subs {
		total += int(s.clear) + int(s.protected)
		protected += int(s.protected)
	}
	if total != len(sample) {
		t.Fatalf("subsample sizes sum to %d, want %d", total, len(sample))
	}
	if protected != 0 {
		t.Fatalf("OBU_FRAME must be treated entirely clear in this version, got %d protected bytes", protected)
	}
}
