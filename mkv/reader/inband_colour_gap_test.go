package reader

import (
	"bytes"
	"context"
	"testing"
)

// TestInBandColourAV1BareRecord: an av1C with no sequence header and no
// container colour leaves the colour to the first temporal unit.
func TestInBandColourAV1BareRecord(t *testing.T) {
	full := mustHex(t, av1HDRPrivateHex)
	tu := append([]byte{0x12, 0x00}, full[4:]...) // temporal delimiter, then the sequence header OBU
	data := inBandColourMKV(full[:4], tu)
	data = bytes.Replace(data, []byte("V_MPEGH/ISO/HEVC"), []byte("V_AV1\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), 1)

	want, err := ReadMeta(context.Background(), bytes.NewReader(bytes.Replace(inBandColourMKV(full, nil),
		[]byte("V_MPEGH/ISO/HEVC"), []byte("V_AV1\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), 1)), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	w := want.Tracks[0]
	if !w.ColourDetermined || w.ColorTransferName() != "smpte2084" {
		t.Fatalf("fixture: the full av1C must state PQ, got determined=%v transfer=%q", w.ColourDetermined, w.ColorTransferName())
	}

	base, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if tr := base.Tracks[0]; tr.ColourDetermined || tr.ColorTransfer != nil {
		t.Fatalf("head only: determined=%v transfer=%v, want none", tr.ColourDetermined, tr.ColorTransfer)
	}
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if !tr.ColourDetermined || tr.ColorTransferName() != w.ColorTransferName() ||
		tr.ColorPrimariesName() != w.ColorPrimariesName() || tr.ColorSpaceName() != w.ColorSpaceName() {
		t.Errorf("determined=%v transfer=%q primaries=%q space=%q, want true %q %q %q", tr.ColourDetermined,
			tr.ColorTransferName(), tr.ColorPrimariesName(), tr.ColorSpaceName(),
			w.ColorTransferName(), w.ColorPrimariesName(), w.ColorSpaceName())
	}
}

// TestInBandColourHEVCRecordWithoutSPS: an hvcC whose arrays hold no SPS (a VPS
// alone here) states no colour either, so the first sample's SPS is read.
func TestInBandColourHEVCRecordWithoutSPS(t *testing.T) {
	sps := extractHEVCSPSNAL(t, mustHex(t, hevcHDRPrivateHex))
	cp := bareHvcC()
	cp[22] = 1
	cp = append(cp, 0x20, 0x00, 0x01, 0x00, 0x04, 0x40, 0x01, 0x0c, 0x01) // one array: a 4-byte VPS
	data := inBandColourMKV(cp, lenPrefixed4(sps))
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Tracks[0]; tr.ColorTransferName() != "smpte2084" || !tr.ColourDetermined {
		t.Errorf("transfer=%q determined=%v, want smpte2084 true", tr.ColorTransferName(), tr.ColourDetermined)
	}
}
