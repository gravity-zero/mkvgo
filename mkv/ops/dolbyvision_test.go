package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
		fx := dovitest.Build(t, dir, dovitest.Spec{Full: full, HDR: true})
		src, _ := videoBlocks(t, fx.Path)
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
			var size, rpus, escapes int
			for i := range got {
				size += len(got[i])
				n, e := dovitest.CheckSample(t, got[i], int(fx.Track.CodecPrivate[21]&3)+1)
				rpus, escapes = rpus+n, escapes+e
				if !bytes.Equal(got[i], tc.want[i]) {
					t.Fatalf("conversion %d: block %d differs from the expected layers", tc.conv, i)
				}
			}
			if want := map[bool]int{true: len(got)}[tc.conv == mkv.DolbyVisionToProfile81]; rpus != want || (want > 0 && full && escapes == 0) {
				t.Errorf("conversion %d: %d RPUs in %d blocks with %d escape bytes, want %d RPUs (and escapes for the identity curves)", tc.conv, rpus, len(got), escapes, want)
			}
			out := c.Tracks[0]
			if dv := out.DolbyVision; tc.conv == mkv.DolbyVisionToBaseLayer {
				if dv != nil || len(out.BlockAdditionMappings) != 0 {
					t.Errorf("base layer: track still signals Dolby Vision %+v, mappings %+v", dv, out.BlockAdditionMappings)
				}
			} else if dv == nil || *dv != (mkv.DolbyVision{VersionMajor: 1, Profile: 8, Level: 6, RPUPresent: true, BLPresent: true, BLSignalCompatID: 1}) {
				t.Errorf("profile 8.1: track signals %+v", dv)
			}
			if in := src.Tracks[0]; in.HDR == nil || in.HDR.MasteringDisplay == nil || in.HDR.MaxCLL != 1000 || colourOf(in) != [4]uint16{16, 9, 9, 1} {
				t.Fatalf("the fixture carries no HDR10 description: %+v %v", in.HDR, colourOf(in))
			} else if !reflect.DeepEqual(out.HDR, in.HDR) || !reflect.DeepEqual(colourOf(out), colourOf(in)) {
				t.Errorf("conversion %d: base layer HDR metadata %+v %v, the source has %+v %v", tc.conv, out.HDR, colourOf(out), in.HDR, colourOf(in))
			}
			if n := statTag(c, "NUMBER_OF_BYTES"); n != strconv.Itoa(size) {
				t.Errorf("conversion %d: NUMBER_OF_BYTES tag %q, wrote %d bytes", tc.conv, n, size)
			}
		}
	}
}

// colourOf returns a track's transfer, primaries, matrix and range, 0 where unset.
func colourOf(t mkv.Track) (out [4]uint16) {
	for i, p := range []*uint16{t.ColorTransfer, t.ColorPrimaries, t.ColorSpace, t.ColorRange} {
		if p != nil {
			out[i] = *p
		}
	}
	return out
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

// TestConvertDolbyVisionFFprobe: an outside reader parses the converted files to the values the conversion owes -
// the identity curves in place of a full layer's reshaping, a minimal layer's own curves kept, no Dolby Vision left on
// the base layer. Opt-in: MKVGO_FFPROBE names the ffprobe to use.
func TestConvertDolbyVisionFFprobe(t *testing.T) {
	for _, full := range []bool{false, true} {
		dir := t.TempDir()
		fx := dovitest.Build(t, dir, dovitest.Spec{Full: full, HDR: true})
		p81, base := filepath.Join(dir, "p81.mkv"), filepath.Join(dir, "base.mkv")
		if err := errors.Join(ConvertDolbyVision(context.Background(), fx.Path, p81, mkv.DolbyVisionToProfile81),
			ConvertDolbyVision(context.Background(), fx.Path, base, mkv.DolbyVisionToBaseLayer)); err != nil {
			t.Fatal(err)
		}
		dovitest.CheckProfile81(t, fx.Path, p81, full)
		dovitest.CheckBase(t, fx.Path, base)
	}
}

// TestConvertDolbyVisionCRCJudged: the CRC a conversion writes is the one an outside decoder computes. Two one-frame
// files, the conversion's output and a copy with one bit of the RPU's CRC flipped, are decoded apart under CRC
// checking. Opt-in: MKVGO_FFPROBE names the ffprobe to use.
func TestConvertDolbyVisionCRCJudged(t *testing.T) {
	dir := t.TempDir()
	fx := dovitest.Build(t, dir, dovitest.Spec{Full: true, HDR: true, Frames: 1})
	good, bad := filepath.Join(dir, "good.mkv"), filepath.Join(dir, "bad.mkv")
	if err := ConvertDolbyVision(context.Background(), fx.Path, good, mkv.DolbyVisionToProfile81); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	lenSize := int(fx.Track.CodecPrivate[21]&3) + 1
	at := bytes.Index(b, []byte{0x7C, 0x01, 0x19})
	if at < lenSize || bytes.Count(b, []byte{0x7C, 0x01, 0x19}) != 1 {
		t.Fatalf("the output holds %d RPUs, want the one of its single frame", bytes.Count(b, []byte{0x7C, 0x01, 0x19}))
	}
	n := 0
	for _, c := range b[at-lenSize : at] {
		n = n<<8 | int(c)
	}
	end := at + n // the RPU closes on its CRC and 0x80
	flipped := false
	for i := end - 2; i >= end-5 && !flipped; i-- {
		for _, mask := range []byte{0x40, 0x80} {
			if b[i] > 3 && b[i]^mask > 3 { // neither an escape nor the making of one
				b[i], flipped = b[i]^mask, true
				break
			}
		}
	}
	if !flipped || b[end-1] != 0x80 {
		t.Fatalf("no CRC byte to flip in % x", b[end-6:end])
	}
	if err := os.WriteFile(bad, b, 0o644); err != nil {
		t.Fatal(err)
	}
	dovitest.CheckCRCJudged(t, good, bad)
}
