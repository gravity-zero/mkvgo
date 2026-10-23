package mp4

import (
	"encoding/binary"
	"testing"
)

// A count whose count*entrySize wraps a 32-bit int to a few bytes passed the length checks on 386/arm, then panicked.
func TestTableCountOverflow32Bit(t *testing.T) {
	table := func(count uint32, size int) []byte {
		p := make([]byte, size)
		binary.BigEndian.PutUint32(p[4:8], count)
		return p
	}
	stsz := make([]byte, 24)
	binary.BigEndian.PutUint32(stsz[8:12], 0x00800000)
	cases := []struct {
		name string
		run  func() error
	}{
		{"stsz", func() error { _, err := parseStsz(stsz, 0); return err }},
		{"stco", func() error {
			_, err := parseChunkOffsets([]memBox{{typ: "stco", payload: table(0x40000000, 24)}})
			return err
		}},
		{"co64", func() error {
			_, err := parseChunkOffsets([]memBox{{typ: "co64", payload: table(0x20000000, 24)}})
			return err
		}},
		{"stsc", func() error { _, err := parseStsc(table(0x15555556, 24)); return err }},
		{"stts", func() error { _, err := parseStts(table(0x20000000, 24), 4); return err }},
		{"sttsRuns", func() error { _, err := newSttsRuns(table(0x20000000, 24), 4); return err }},
		{"ctts", func() error {
			for _, v := range parseCtts([]memBox{{typ: "ctts", payload: table(0x20000000, 24)}}, 4) {
				if v != 0 {
					t.Errorf("ctts: offset %d from a table too short for its count", v)
				}
			}
			return errf("ctts ignored")
		}},
		{"vp9 superframe", func() error {
			_, _, err := vp9SplitSuperframe([]byte{0x82, 0x49, 0x83, 0xD8, 0xFF, 0xFF, 0xFF, 0xFF, 0xD8})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); err == nil {
				t.Fatal("a table too short for its declared count was accepted")
			}
		})
	}
}
