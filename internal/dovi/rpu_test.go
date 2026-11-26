package dovi_test

import (
	"bytes"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi"
	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
)

func TestRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		rpu  []byte
		want dovi.RPU
	}{
		{"full layer", dovitest.RPU(true), dovi.RPU{Profile: 7, EL: dovi.ELFull}},
		{"minimal layer", dovitest.RPU(false), dovi.RPU{Profile: 7, EL: dovi.ELMinimal}},
		{"profile 8.1", dovitest.RPU81(false), dovi.RPU{Profile: 8}},
	} {
		if got, err := dovi.Read(tc.rpu); err != nil || got != tc.want {
			t.Errorf("%s: got %+v, err %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

// TestTo81: a profile 7 RPU converts, bit for bit, to the profile 8.1 RPU built field by field - curves kept under a
// minimal enhancement layer, identity under a full one - and a converted RPU converts to itself.
func TestTo81(t *testing.T) {
	for _, full := range []bool{false, true} {
		want := dovitest.RPU81(full)
		for _, src := range [][]byte{dovitest.RPU(full), want, append(dovitest.RPU(full), 0, 0)} {
			got, err := dovi.To81(src)
			if err != nil {
				t.Fatalf("full=%v: %v", full, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("full=%v: converted RPU differs\n got %x\nwant %x", full, got, want)
			}
		}
	}
}

func TestTo81Refuses(t *testing.T) {
	good := dovitest.RPU(true)
	flipped := bytes.Clone(good)
	flipped[20] ^= 1
	for name, rpu := range map[string][]byte{"checksum": flipped, "empty": nil, "no final byte": good[:len(good)-1]} {
		if out, err := dovi.To81(rpu); err == nil {
			t.Errorf("%s: converted to %x, want an error", name, out)
		}
	}
	for cut := range good {
		if _, err := dovi.To81(good[:cut]); err == nil {
			t.Fatalf("RPU cut at %d bytes converted", cut)
		}
	}
}

// mpeg2CRC is the checksum an RPU closes on.
func mpeg2CRC(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, c := range b {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func FuzzTo81(f *testing.F) {
	for _, full := range []bool{false, true} {
		f.Add(dovitest.RPU(full))
		f.Add(dovitest.RPU81(full))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 9 {
			return
		}
		body := bytes.Clone(b[:len(b)-5])
		body[0] = 0x19
		crc := mpeg2CRC(body[1:])
		out, err := dovi.To81(append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc), 0x80))
		if err != nil {
			return
		}
		if r, err := dovi.Read(out); err != nil || r.Profile != 8 || r.EL != dovi.ELNone {
			t.Fatalf("converted RPU reads as %+v, err %v", r, err)
		}
		if again, err := dovi.To81(out); err != nil || !bytes.Equal(again, out) {
			t.Fatalf("a converted RPU does not convert to itself: %v", err)
		}
	})
}
