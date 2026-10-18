package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"strings"
	"testing"
)

// withDeclaredBitrates gives a file what mainstream muxers write per file: a btrt in the video entry and an esds avgBitrate.
func withDeclaredBitrates(t *testing.T, path string, rate uint32) {
	t.Helper()
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(_ int, trak []byte) []byte {
			out, err := rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
				var kids [][]byte
				for _, b := range sb {
					if b.typ != "stsd" {
						kids = append(kids, box(b.typ, b.payload))
						continue
					}
					entries, err := iterBoxes(b.payload[8:])
					if err != nil {
						return nil, err
					}
					var w bw
					w.bytes(b.payload[:8])
					for _, e := range entries {
						payload := append([]byte(nil), e.payload...)
						switch e.typ {
						case "avc1":
							btrt := box("btrt", binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 0), rate), rate))
							payload = append(payload, btrt...)
						case "mp4a":
							// objectTypeIndication 0x40, streamType 0x15, bufferSizeDB, maxBitrate, avgBitrate: all zero as mkvgo writes them.
							zero := []byte{0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
							i := bytes.Index(payload, zero)
							if i < 0 {
								t.Fatal("no esds DecoderConfigDescriptor to patch")
							}
							binary.BigEndian.PutUint32(payload[i+5:], rate)
							binary.BigEndian.PutUint32(payload[i+9:], rate)
						}
						w.bytes(box(e.typ, payload))
					}
					kids = append(kids, box("stsd", w.b))
				}
				return kids, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		})
	})
}

// TestJoinIgnoresPerFileBitrates: clips of one encoder setting differ in the bitrate their entries declare, not in how they decode.
func TestJoinIgnoresPerFileBitrates(t *testing.T) {
	a, b := editFixture(t), editFixture(t)
	withDeclaredBitrates(t, b, 800_000)
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), []string{a, b}, dst); err != nil {
		t.Errorf("join of clips differing only in declared bitrate: %v", err)
	}
	// A decoder configuration that really differs (another AudioSpecificConfig) is still refused.
	c := editFixture(t)
	rewriteTailMoov(t, c, func(p []byte) []byte {
		i := bytes.Index(p, fakeASC)
		if i < 0 {
			t.Fatal("no AudioSpecificConfig to change")
		}
		p = append([]byte(nil), p...)
		p[i+1] ^= 0x08 // another channel configuration
		return p
	})
	if err := Join(context.Background(), []string{a, c}, filepath.Join(t.TempDir(), "k.mp4")); err == nil || !strings.Contains(err.Error(), "track 2 is encoded differently") {
		t.Errorf("join of clips with another AudioSpecificConfig: %v", err)
	}
}
