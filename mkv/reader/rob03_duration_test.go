package reader

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestROB03_ZeroLengthDuration(t *testing.T) {
	data, err := os.ReadFile("../../internal/testdata/sample.mkv")
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(data, []byte{0x44, 0x89, 0x88})
	if i < 0 {
		t.Fatal("no 8-byte Duration in the fixture")
	}
	// Duration of length 0, then a Void filling the 8 bytes freed, so no size moves.
	copy(data[i:], []byte{0x44, 0x89, 0x80, 0xEC, 0x86, 0, 0, 0, 0, 0, 0})
	for name, read := range map[string]func() (float64, error){
		"Read": func() (float64, error) {
			c, err := Read(context.Background(), bytes.NewReader(data), "zero.mkv")
			if err != nil {
				return 0, err
			}
			return c.Info.Duration, nil
		},
		"ReadMeta": func() (float64, error) {
			c, err := ReadMeta(context.Background(), bytes.NewReader(data), "zero.mkv")
			if err != nil {
				return 0, err
			}
			return c.Info.Duration, nil
		},
	} {
		d, err := read()
		if err != nil {
			t.Fatalf("%s: a zero-length Duration fails the file: %v", name, err)
		}
		if d != 0 {
			t.Fatalf("%s: Duration %v, want 0", name, d)
		}
	}
}
