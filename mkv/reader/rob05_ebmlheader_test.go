package reader

import (
	"bytes"
	"testing"
)

func TestROB05_EBMLHeaderSize(t *testing.T) {
	cases := map[string][]byte{
		"unknown size": {0x1A, 0x45, 0xDF, 0xA3, 0xFF},
		"2^40":         {0x1A, 0x45, 0xDF, 0xA3, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00},
		"past 1 MiB":   {0x1A, 0x45, 0xDF, 0xA3, 0x10, 0x10, 0x00, 0x01},
	}
	for name, head := range cases {
		data := append(append([]byte{}, head...), make([]byte, 4<<20)...)
		g := &overReadGuard{r: bytes.NewReader(data), budget: 1 << 30}
		if _, err := NewBlockReader(g, 1_000_000); err == nil {
			t.Errorf("%s: an EBML header of that size was accepted", name)
		}
		if g.read >= 64 {
			t.Errorf("%s: %d bytes read before refusing the header", name, g.read)
		}
	}
}
