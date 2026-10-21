package mp4

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An MPEG-TS (or M2TS) file named .mp4 is refused as MPEG-TS, still ErrNotMP4
// for a caller that routes other containers; any other non-MP4 is not called
// MPEG-TS.
func TestMPEGTSRefusedByName(t *testing.T) {
	packets := func(prefix, stride int) []byte {
		var b bytes.Buffer
		for i := 0; i < 20; i++ {
			p := make([]byte, stride)
			p[prefix] = 0x47
			b.Write(p)
		}
		return b.Bytes()
	}
	for _, c := range []struct {
		name string
		data []byte
		ts   bool
	}{
		{"ts", packets(0, 188), true},
		{"m2ts", packets(4, 192), true},
		{"other", bytes.Repeat([]byte("not a box "), 100), false},
	} {
		path := filepath.Join(t.TempDir(), c.name+".mp4")
		if err := os.WriteFile(path, c.data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := PlanHLS(context.Background(), path, Options{})
		if !errors.Is(err, ErrNotMP4) {
			t.Errorf("%s: %v, want ErrNotMP4", c.name, err)
		}
		if errors.Is(err, ErrMPEGTS) != c.ts || strings.Contains(err.Error(), "MPEG-TS") != c.ts {
			t.Errorf("%s: %v, MPEG-TS said %v, want %v", c.name, err, errors.Is(err, ErrMPEGTS), c.ts)
		}
	}
}
