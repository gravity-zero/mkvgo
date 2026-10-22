package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// An MPEG-TS or M2TS handed to the repairs or to the triage is refused by name, mkv.ErrMPEGTS: repairing it would
// mean remuxing it, and it is no MP4 either.
func TestRepairsRefuseMPEGTS(t *testing.T) {
	packets := func(prefix, stride int) []byte {
		b := make([]byte, 20*stride)
		for i := range 20 {
			b[i*stride+prefix] = 0x47
		}
		return b
	}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		data []byte
	}{{"ts", packets(0, 188)}, {"m2ts", packets(4, 192)}} {
		dir := t.TempDir()
		src := filepath.Join(dir, c.name+".mkv")
		if err := os.WriteFile(src, c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		runs := map[string]func() error{
			"Reindex":        func() error { return Reindex(ctx, src, filepath.Join(dir, "r.mkv")) },
			"ReindexInPlace": func() error { return ReindexInPlace(ctx, src) },
			"Salvage":        func() error { _, err := Salvage(ctx, src, filepath.Join(dir, "s.mkv")); return err },
			"Diagnose":       func() error { _, err := Diagnose(ctx, src); return err },
		}
		for name, run := range runs {
			if err := run(); !errors.Is(err, mkv.ErrMPEGTS) {
				t.Errorf("%s %s: %v, want mkv.ErrMPEGTS", name, c.name, err)
			}
		}
	}
}
