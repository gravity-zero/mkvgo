package mp4

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestHDRStaticMP4 covers reading HDR10 static metadata from a visual sample
// entry's clli (Content Light Level) and mdcv (Mastering Display, SMPTE ST 2086)
// boxes, including the mdcv G,B,R primary order and fixed-point → unit conversion.
func TestHDRStaticMP4(t *testing.T) {
	clli := box("clli", []byte{0x03, 0xE8, 0x01, 0x90}) // MaxCLL=1000, MaxFALL=400

	mdcv := make([]byte, 24)
	put16 := func(off int, v uint16) { binary.BigEndian.PutUint16(mdcv[off:], v) }
	put32 := func(off int, v uint32) { binary.BigEndian.PutUint32(mdcv[off:], v) }
	// Primaries G, B, R (chromaticity × 50000); white point; max/min lum (× 10000).
	put16(0, 8500)
	put16(2, 39850) // green 0.170, 0.797
	put16(4, 6550)
	put16(6, 2300) // blue 0.131, 0.046
	put16(8, 35400)
	put16(10, 14600) // red 0.708, 0.292
	put16(12, 15635)
	put16(14, 16450) // white 0.3127, 0.3290
	put32(16, 10_000_000)
	put32(20, 50) // lum 1000, 0.005

	payload := make([]byte, 78) // visual sample entry fixed header
	payload = append(payload, box("hvcC", []byte{1, 2, 3})...)
	payload = append(payload, clli...)
	payload = append(payload, box("mdcv", mdcv)...)

	var tr inTrack
	parseHDRStatic(&tr, payload, 78)

	if tr.hdr == nil {
		t.Fatal("hdr is nil, want populated")
	}
	if tr.hdr.MaxCLL != 1000 || tr.hdr.MaxFALL != 400 {
		t.Errorf("MaxCLL/MaxFALL = %d/%d, want 1000/400", tr.hdr.MaxCLL, tr.hdr.MaxFALL)
	}
	md := tr.hdr.MasteringDisplay
	if md == nil {
		t.Fatal("MasteringDisplay is nil")
	}
	for _, tc := range []struct {
		name      string
		got, want float64
	}{
		{"RedX", md.RedX, 0.708}, {"RedY", md.RedY, 0.292},
		{"GreenX", md.GreenX, 0.170}, {"GreenY", md.GreenY, 0.797},
		{"BlueX", md.BlueX, 0.131}, {"BlueY", md.BlueY, 0.046},
		{"WhiteX", md.WhiteX, 0.3127}, {"WhiteY", md.WhiteY, 0.3290},
		{"LumMax", md.LuminanceMax, 1000.0}, {"LumMin", md.LuminanceMin, 0.005},
	} {
		if math.Abs(tc.got-tc.want) > 1e-6 {
			t.Errorf("%s = %g, want %g", tc.name, tc.got, tc.want)
		}
	}
}
