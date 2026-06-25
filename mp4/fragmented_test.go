package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// box4 builds a 32-bit-size box: [size][type][body].
func box4(typ string, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

// TestFragmentedFlag proves Container.Fragmented signals a fragmented MP4 (an
// mvex box in the moov) and stays false for a regular one. It reuses a real
// moov (valid tracks) and appends an mvex box, since the writer never produces a
// fragmented MP4.
func TestFragmentedFlag(t *testing.T) {
	data, err := os.ReadFile(buildTestMP4(t))
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	full, err := readMoov(bytes.NewReader(data), size)
	if err != nil {
		t.Fatal(err)
	}

	// Regular moov: no mvex → not fragmented.
	reg, err := parseMoov(full, size, sampleKeyframes)
	if err != nil {
		t.Fatal(err)
	}
	if containerFromMovie(reg).Fragmented {
		t.Error("a regular MP4 must not be flagged fragmented")
	}

	// Same moov plus an mvex box → fragmented, tracks still parsed.
	frag := append(append([]byte{}, full...), box4("mvex", box4("trex", make([]byte, 20)))...)
	fmv, err := parseMoov(frag, size, sampleKeyframes)
	if err != nil {
		t.Fatal(err)
	}
	fc := containerFromMovie(fmv)
	if !fc.Fragmented {
		t.Error("an MP4 with an mvex box must be flagged fragmented")
	}
	if len(fc.Tracks) == 0 {
		t.Fatal("fragmented fixture lost its tracks")
	}
}
