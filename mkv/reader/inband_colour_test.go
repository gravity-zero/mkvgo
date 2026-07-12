package reader

import (
	"bytes"
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// bareHvcC builds a 23-byte HEVCDecoderConfigurationRecord with numOfArrays==0  -
// the "header only, no SPS" hvcC that streaming-style HDR muxes write, keeping
// the parameter sets in-band. Main 10, 10-bit, 4-byte NAL length prefix.
func bareHvcC() []byte {
	cp := make([]byte, 23)
	cp[0] = 1     // configurationVersion
	cp[1] = 2     // general_profile_idc = 2 (Main 10)
	cp[17] = 2    // bitDepthLumaMinus8 = 2 → 10-bit
	cp[18] = 2    // bitDepthChromaMinus8 = 2
	cp[21] = 0xff // …lengthSizeMinusOne (low 2 bits) = 3 → 4-byte NAL prefix
	cp[22] = 0    // numOfArrays = 0
	return cp
}

// extractHEVCSPSNAL pulls the SPS NAL (with its 2-byte header) out of a hvcC that
// does carry NAL arrays, so a real HDR SPS can be replanted in-band.
func extractHEVCSPSNAL(t *testing.T, hvcC []byte) []byte {
	t.Helper()
	if len(hvcC) < 23 || hvcC[0] != 1 {
		t.Fatal("bad hvcC")
	}
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

// lenPrefixed4 length-prefixes a NAL with a 4-byte big-endian size (matching a
// hvcC lengthSizeMinusOne of 3).
func lenPrefixed4(nal []byte) []byte {
	n := len(nal)
	return append([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}, nal...)
}

// inBandColourMKV builds an MKV with one HEVC video track carrying hvcC as
// CodecPrivate (no container Colour element) and a single cluster whose first
// SimpleBlock holds the given access-unit bytes.
func inBandColourMKV(hvcC, frame []byte) []byte {
	te := trackEntry(
		uintElem(mkv.IDTrackNumber, 1, 1),
		uintElem(mkv.IDTrackType, mkv.TrackTypeVideo, 1),
		strElem(mkv.IDCodecID, "V_MPEGH/ISO/HEVC"),
		bytesElem(mkv.IDCodecPrivate, hvcC),
		masterElem(mkv.IDVideo,
			uintElem(mkv.IDPixelWidth, 3840, 2),
			uintElem(mkv.IDPixelHeight, 2160, 2),
		),
	)
	info := masterElem(mkv.IDInfo, uintElem(mkv.IDTimecodeScale, 1_000_000, 4))
	tracks := masterElem(mkv.IDTracks, te)

	block := append([]byte{0x81, 0x00, 0x00, 0x80}, frame...) // track 1, relTC 0, keyframe
	cluster := clusterWithSimpleBlock(block)

	var seg bytes.Buffer
	seg.Write(info)
	seg.Write(tracks)
	ebml.WriteElementHeader(&seg, mkv.IDCluster, int64(len(cluster)))
	seg.Write(cluster)

	var buf bytes.Buffer
	writeEBMLHeader(&buf)
	writeSegmentStart(&buf, int64(seg.Len()))
	buf.Write(seg.Bytes())
	return buf.Bytes()
}

// TestInBandColourFallback proves the opt-in recovers colour from an in-band SPS
// (bare hvcC, no container Colour), while the default head-only read leaves it
// nil - and never reads the cluster.
func TestInBandColourFallback(t *testing.T) {
	sps := extractHEVCSPSNAL(t, mustHex(t, hevcHDRPrivateHex))
	vps := []byte{0x40, 0x01, 0xde, 0xad} // dummy VPS (type 32) before the SPS - must be skipped
	frame := append(lenPrefixed4(vps), lenPrefixed4(sps)...)
	data := inBandColourMKV(bareHvcC(), frame)

	// Default: head-only, bare hvcC → no colour, cluster untouched.
	base, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if tr := base.Tracks[0]; tr.ColorTransfer != nil || tr.ColorPrimaries != nil || tr.ColorSpace != nil {
		t.Fatalf("without the option colour must stay nil, got space=%v transfer=%v primaries=%v",
			tr.ColorSpace, tr.ColorTransfer, tr.ColorPrimaries)
	}

	// Opted-in: the first sample's SPS is read and its VUI parsed.
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.ColorSpaceName() != "bt2020nc" || tr.ColorTransferName() != "smpte2084" ||
		tr.ColorPrimariesName() != "bt2020" || !tr.IsHDR() {
		t.Errorf("in-band colour: space=%q transfer=%q primaries=%q hdr=%v, want bt2020nc/smpte2084/bt2020/true",
			tr.ColorSpaceName(), tr.ColorTransferName(), tr.ColorPrimariesName(), tr.IsHDR())
	}
}

// atcSEINAL builds an Alternative Transfer Characteristics SEI NAL (prefix SEI,
// payload type 147) carrying preferred_transfer_characteristics = transfer.
func atcSEINAL(transfer byte) []byte {
	// NAL header: nal_unit_type 39 (PREFIX_SEI_NUT) → 0x4e, layer 0 / tid+1 = 1.
	// RBSP: payloadType=147 (0x93), payloadSize=1 (0x01), payload, trailing 0x80.
	return []byte{0x4e, 0x01, 0x93, 0x01, transfer, 0x80}
}

// TestInBandColourATCSEIOverride proves the HLG case: the SPS VUI advertises a
// transfer for legacy decoders, and an Alternative Transfer Characteristics SEI
// in the same access unit overrides it with the real value (here arib-std-b67),
// while primaries/matrix stay as the SPS gave them.
func TestInBandColourATCSEIOverride(t *testing.T) {
	sps := extractHEVCSPSNAL(t, mustHex(t, hevcHDRPrivateHex)) // SPS VUI transfer = smpte2084
	frame := append(lenPrefixed4(sps), lenPrefixed4(atcSEINAL(18))...)
	data := inBandColourMKV(bareHvcC(), frame)

	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracks[0]
	if tr.ColorTransferName() != "arib-std-b67" {
		t.Errorf("transfer = %q, want arib-std-b67 (the ATC SEI must override the SPS VUI)", tr.ColorTransferName())
	}
	if tr.ColorSpaceName() != "bt2020nc" || tr.ColorPrimariesName() != "bt2020" || !tr.IsHDR() {
		t.Errorf("space/primaries/hdr must come from the SPS: space=%q primaries=%q hdr=%v, want bt2020nc/bt2020/true",
			tr.ColorSpaceName(), tr.ColorPrimariesName(), tr.IsHDR())
	}
}

// TestInBandColourFallbackNoOpWhenColourPresent proves the option does not read a
// frame when colour is already known: a hvcC that DOES carry its SPS yields colour
// head-only, so needsInBandColour is false and the cluster is never touched. A
// reader that fails on any read past the head would catch a stray frame read.
func TestInBandColourFallbackNoOpWhenColourPresent(t *testing.T) {
	// hvcC with the SPS in its NAL arrays → colour comes from the codec private.
	data := inBandColourMKV(mustHex(t, hevcHDRPrivateHex), nil)
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Tracks[0]; tr.ColorTransferName() != "smpte2084" {
		t.Errorf("colour should come from the codec-private SPS: transfer=%q", tr.ColorTransferName())
	}
}

// TestInBandColourFallbackRobust: the option must never error or panic on a file
// with no usable first sample (here, a bare hvcC but an empty cluster); colour
// simply stays nil, exactly as the default.
func TestInBandColourFallbackRobust(t *testing.T) {
	data := inBandColourMKV(bareHvcC(), nil) // SimpleBlock with no NAL payload
	c, err := ReadMeta(context.Background(), bytes.NewReader(data), "x.mkv", WithInBandColourFallback())
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Tracks[0]; tr.ColorTransfer != nil {
		t.Errorf("no SPS in the frame → colour must stay nil, got transfer=%v", tr.ColorTransfer)
	}
}
