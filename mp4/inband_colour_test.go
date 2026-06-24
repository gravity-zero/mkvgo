package mp4

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// A real HEVC HDR hvcC (Main 10, bt2020nc / PQ / 10-bit) WITH its SPS in the NAL
// arrays — the SPS is lifted out of it and replanted in-band for these tests.
const hevcHDRPrivateHex = "0102200000009000000000001ef000fcfdfafa00000f03200001001840010c01ffff02200000030090000003000003001e959809210001002d42010102200000030090000003000003001ea0208104d96566924caf016a12201208000003000800000300c04022000100074401c172b42240"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	return b
}

// bareHvcC is a 23-byte HEVCDecoderConfigurationRecord with numOfArrays==0 (no
// SPS), as streaming-style muxes write when the parameter sets live in-band.
func bareHvcC() []byte {
	cp := make([]byte, 23)
	cp[0] = 1     // version
	cp[1] = 2     // Main 10
	cp[17] = 2    // bitDepthLumaMinus8 → 10-bit
	cp[18] = 2    // bitDepthChromaMinus8
	cp[21] = 0xff // lengthSizeMinusOne (low 2 bits) = 3 → 4-byte NAL prefix
	cp[22] = 0    // numOfArrays = 0
	return cp
}

func extractHEVCSPSNAL(t *testing.T, hvcC []byte) []byte {
	t.Helper()
	off := 23
	for a := 0; a < int(hvcC[22]); a++ {
		nalType := hvcC[off] & 0x3f
		count := int(hvcC[off+1])<<8 | int(hvcC[off+2])
		off += 3
		for n := 0; n < count; n++ {
			ln := int(hvcC[off])<<8 | int(hvcC[off+1])
			off += 2
			nal := hvcC[off : off+ln]
			off += ln
			if nalType == 33 {
				return nal
			}
		}
	}
	t.Fatal("no SPS NAL in hvcC")
	return nil
}

func lenPrefixed4(nal []byte) []byte {
	n := len(nal)
	return append([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, nal...)
}

// atcSEINAL builds an Alternative Transfer Characteristics SEI NAL (prefix SEI,
// payload type 147) carrying preferred_transfer_characteristics = transfer.
func atcSEINAL(transfer byte) []byte {
	return []byte{0x4e, 0x01, 0x93, 0x01, transfer, 0x80}
}

// TestMP4InBandColourFallback proves the MP4 counterpart of the MKV in-band
// fallback: an HEVC track with a bare hvcC and no colr box keeps its colour
// in-band; the opt-in reads the first sample and recovers the SPS VUI plus the
// ATC SEI override (here arib-std-b67), while the default leaves colour nil.
func TestMP4InBandColourFallback(t *testing.T) {
	sps := extractHEVCSPSNAL(t, mustHex(t, hevcHDRPrivateHex)) // SPS VUI transfer = PQ
	frame := append(lenPrefixed4(sps), lenPrefixed4(atcSEINAL(18))...)

	w, h := uint32(3840), uint32(2160)
	tracks := []mkv.Track{{
		ID: 1, Type: mkv.VideoTrack, Codec: "V_MPEGH/ISO/HEVC",
		CodecPrivate: bareHvcC(), Width: &w, Height: &h,
	}}
	mkvPath := buildMKV(t, tracks, []genBlock{{track: 1, pts: 0, key: true, data: frame}})
	mp4Bytes, _ := remux(t, mkvPath)

	// Default: head-only, bare hvcC + no colr → no colour.
	base, _, err := ReadMeta(context.Background(), bytes.NewReader(mp4Bytes), "x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Tracks) == 0 {
		t.Fatal("video track was dropped by the remux")
	}
	if tr := base.Tracks[0]; tr.ColorTransfer != nil || tr.ColorSpace != nil {
		t.Fatalf("without the option colour must stay nil, got transfer=%v space=%v", tr.ColorTransfer, tr.ColorSpace)
	}

	// Opted-in: the first sample's SPS + ATC SEI are read.
	c, _, err := ReadMeta(context.Background(), bytes.NewReader(mp4Bytes), "x.mp4", Options{InBandColour: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.ColorSpaceName() != "bt2020nc" || tr.ColorTransferName() != "arib-std-b67" ||
		tr.ColorPrimariesName() != "bt2020" || !tr.IsHDR() {
		t.Errorf("MP4 in-band colour: space=%q transfer=%q primaries=%q hdr=%v, want bt2020nc/arib-std-b67/bt2020/true",
			tr.ColorSpaceName(), tr.ColorTransferName(), tr.ColorPrimariesName(), tr.IsHDR())
	}
}
