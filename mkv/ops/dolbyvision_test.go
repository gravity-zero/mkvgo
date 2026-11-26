package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// videoBlocks reads a file's tracks and the payloads of its first track's blocks.
func videoBlocks(t *testing.T, path string) (*mkv.Container, [][]byte) {
	t.Helper()
	c, err := reader.Open(context.Background(), path, reader.WithTags())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for {
		b, err := br.Next()
		if err != nil {
			break
		}
		out = append(out, append([]byte(nil), b.Data...))
	}
	return c, out
}

func TestConvertDolbyVision(t *testing.T) {
	for _, full := range []bool{false, true} {
		dir := t.TempDir()
		fx := dovitest.Profile7(t, dir, full)
		for _, tc := range []struct {
			conv mkv.DolbyVisionConversion
			want [][]byte
		}{{mkv.DolbyVisionToProfile81, fx.Profile81}, {mkv.DolbyVisionToBaseLayer, fx.Base}} {
			dst := filepath.Join(dir, "out"+strconv.Itoa(int(tc.conv))+".mkv")
			if err := ConvertDolbyVision(context.Background(), fx.Path, dst, tc.conv); err != nil {
				t.Fatal(err)
			}
			c, got := videoBlocks(t, dst)
			if len(got) != len(tc.want) || len(got) == 0 {
				t.Fatalf("conversion %d: %d blocks, want %d", tc.conv, len(got), len(tc.want))
			}
			var size int
			for i := range got {
				size += len(got[i])
				if !bytes.Equal(got[i], tc.want[i]) {
					t.Fatalf("conversion %d: block %d differs from the expected layers", tc.conv, i)
				}
			}
			dv := c.Tracks[0].DolbyVision
			if tc.conv == mkv.DolbyVisionToBaseLayer {
				if dv != nil {
					t.Errorf("base layer: track still signals Dolby Vision %+v", dv)
				}
			} else if dv == nil || dv.Profile != 8 || dv.BLSignalCompatID != 1 || dv.ELPresent || !dv.RPUPresent || dv.Level != 6 {
				t.Errorf("profile 8.1: track signals %+v", dv)
			}
			if n := statTag(c, "NUMBER_OF_BYTES"); n != strconv.Itoa(size) {
				t.Errorf("conversion %d: NUMBER_OF_BYTES tag %q, wrote %d bytes", tc.conv, n, size)
			}
		}
	}
}

func statTag(c *mkv.Container, name string) string {
	for _, tag := range c.Tags {
		for _, st := range tag.SimpleTags {
			if st.Name == name {
				return st.Value
			}
		}
	}
	return ""
}

// TestConvertDolbyVisionRefuses: a source with nothing to convert and a damaged RPU both fail, the latter by type,
// and neither leaves an output.
func TestConvertDolbyVisionRefuses(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mkv")
	if err := ConvertDolbyVision(context.Background(), buildTestMKV(t, dir), dst, mkv.DolbyVisionToProfile81); err == nil {
		t.Error("a file with no Dolby Vision track converted")
	}
	fx := dovitest.Profile7(t, dir, true)
	b, err := os.ReadFile(fx.Path)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.LastIndex(b, []byte{0x7C, 0x01, 0x19})
	b[at+12] ^= 0xFF
	if err := os.WriteFile(fx.Path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var dvErr *mkv.DolbyVisionError
	if err := ConvertDolbyVision(context.Background(), fx.Path, dst, mkv.DolbyVisionToProfile81); !errors.As(err, &dvErr) {
		t.Fatalf("damaged RPU: got %v, want a *mkv.DolbyVisionError", err)
	}
	if dvErr.Track != 1 {
		t.Errorf("error names track %d", dvErr.Track)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a failed conversion left an output")
	}
	if err := ConvertDolbyVision(context.Background(), fx.Path, dst, mkv.DolbyVisionToBaseLayer); err != nil {
		t.Errorf("the base layer alone does not read the RPU: %v", err)
	}
}
