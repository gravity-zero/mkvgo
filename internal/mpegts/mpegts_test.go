package mpegts

import (
	"bytes"
	"testing"
)

func packets(prefix, stride, n int) []byte {
	var b bytes.Buffer
	for range n {
		p := make([]byte, stride)
		p[prefix] = 0x47
		b.Write(p)
	}
	return b.Bytes()
}

// TestDetect: three sync bytes a packet apart name a transport stream (188-byte packets) or an M2TS (192, after a
// 4-byte timecode), read from a seeker or from bytes already read; one or two in a row, or anything else, do not.
func TestDetect(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want bool
	}{
		{"ts", packets(0, 188, 20), true},
		{"m2ts", packets(4, 192, 20), true},
		{"two packets", packets(0, 188, 2), false},
		{"one sync byte", append([]byte{0x47}, make([]byte, 1000)...), false},
		{"text", bytes.Repeat([]byte("not a box "), 100), false},
		{"empty", nil, false},
	} {
		if got := At(bytes.NewReader(c.data), int64(len(c.data))); got != c.want {
			t.Errorf("%s: At %v, want %v", c.name, got, c.want)
		}
		if got := At(bytes.NewReader(c.data), -1); got != c.want {
			t.Errorf("%s: At with no size %v, want %v", c.name, got, c.want)
		}
		if got := Prefix(c.data); got != c.want {
			t.Errorf("%s: Prefix %v, want %v", c.name, got, c.want)
		}
	}
	if !Prefix(packets(4, 192, 20)[:PrefixLen]) {
		t.Errorf("an M2TS cut to PrefixLen bytes is not recognised")
	}
}
