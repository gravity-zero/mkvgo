package mp4

import "testing"

// FuzzVP9FrameHeader feeds arbitrary bytes to the VP9 uncompressed-header
// parser (it runs on the first sample of any Matroska track labelled vp9, i.e.
// attacker-controlled data). It must never panic, and a successful parse must
// yield spec-valid field values.
func FuzzVP9FrameHeader(f *testing.F) {
	f.Add([]byte{})
	f.Add(vp9Profile0Key)
	f.Add([]byte{0x92, 0x49, 0x83, 0x42, 0x18, 0x00}) // profile 2, 10-bit
	f.Add([]byte{0x82, 0x49, 0x83, 0x42})             // truncated colour config
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := parseVP9FrameHeader(data)
		if err != nil {
			return
		}
		if h.profile > 3 {
			t.Fatalf("profile = %d, out of the 2-bit range", h.profile)
		}
		switch h.bitDepth {
		case 8, 10, 12:
		default:
			t.Fatalf("bitDepth = %d, want 8/10/12", h.bitDepth)
		}
		if h.chroma > 3 {
			t.Fatalf("chroma = %d, out of the vpcC range", h.chroma)
		}
	})
}
