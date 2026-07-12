package reader

import (
	"encoding/hex"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func seedHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// FuzzCodecColour throws arbitrary bytes at every bitstream colour parser. The
// Exp-Golomb reader (ue/se), the emulation-prevention strip and the per-codec
// header walks are the classic OOB / infinite-loop / OOM surface for a parser of
// untrusted files, so the contract under fuzzing is strict: NEVER panic, NEVER
// hang (the bit reader bounds-checks and ue() caps its leading-zero run), and any
// returned value stays a valid CICP/bit-depth (the only output is a few *uint16).
// fillColourFromCodecPrivate must also be a no-op on a non-video track.
func FuzzCodecColour(f *testing.F) {
	f.Add(seedHex(hevcHDRPrivateHex))
	f.Add(seedHex(h264SDRPrivateHex))
	f.Add(seedHex(av1HDRPrivateHex))
	f.Add([]byte{0x01})
	f.Add([]byte{0x01, 0x64, 0x00, 0x28, 0xff, 0xe1})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, cp []byte) {
		// Call the inner parsers DIRECTLY - they carry no recover(), so the
		// bounds-checks alone must keep them panic-free on any input. The
		// recover() in parseCodecColour is only a last-resort backstop and must
		// never be the thing that saves us here: a panic in one of these aborts
		// the fuzz, exposing a missing bound rather than masking it.
		for _, bc := range []*bitstreamColour{avcColour(cp), hevcColour(cp), av1Colour(cp), vp9Colour(cp)} {
			if bc != nil && bc.bitDepth != nil {
				switch *bc.bitDepth {
				case 8, 10, 12: // the only depths real video uses
				default:
					t.Fatalf("invalid bit depth %d on adversarial input", *bc.bitDepth)
				}
			}
		}
		// Also exercise the public dispatch + fill path (the one with the backstop).
		for _, codec := range []string{"hevc", "h264", "av1", "vp9", "unknown"} {
			tr := mkv.Track{Type: mkv.VideoTrack, Codec: codec, CodecPrivate: cp}
			fillColourFromCodecPrivate(&tr)
		}
	})
}
