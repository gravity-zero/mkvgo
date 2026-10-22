package reader

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An MPEG-TS or M2TS handed to the Matroska reader is refused by name, mkv.ErrMPEGTS, and not ErrNotMatroska, which
// sends a caller to the MP4 reader; other bytes that are not Matroska are not called MPEG-TS.
func TestReadRefusesMPEGTS(t *testing.T) {
	packets := func(prefix, stride int) []byte {
		var b bytes.Buffer
		for range 20 {
			p := make([]byte, stride)
			p[prefix] = 0x47
			b.Write(p)
		}
		return b.Bytes()
	}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		data []byte
		ts   bool
	}{
		{"ts", packets(0, 188), true},
		{"m2ts", packets(4, 192), true},
		{"other", bytes.Repeat([]byte("not ebml  "), 100), false},
	} {
		reads := map[string]func() error{
			"Read":     func() error { _, err := Read(ctx, bytes.NewReader(c.data), c.name+".mkv"); return err },
			"ReadMeta": func() error { _, err := ReadMeta(ctx, bytes.NewReader(c.data), c.name+".mkv"); return err },
			"ReadStream": func() error {
				_, _, err := ReadStream(ctx, bytes.NewReader(c.data))
				return err
			},
		}
		for name, read := range reads {
			err := read()
			if err == nil {
				t.Fatalf("%s %s: no error", name, c.name)
			}
			said := errors.Is(err, mkv.ErrMPEGTS) && strings.Contains(err.Error(), "MPEG-TS detected")
			if said != c.ts {
				t.Errorf("%s %s: %v, MPEG-TS said %v, want %v", name, c.name, err, said, c.ts)
			}
			if c.ts && errors.Is(err, ErrNotMatroska) {
				t.Errorf("%s %s: %v, an MPEG-TS taken for an MP4", name, c.name, err)
			}
		}
	}
}
