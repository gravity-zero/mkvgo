package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
	"github.com/gravity-zero/mkvgo/mkv"
)

// countedReader counts the bytes and the calls read through it.
type countedReader struct {
	io.ReadSeeker
	bytes, reads int
}

func (c *countedReader) Read(p []byte) (int, error) {
	n, err := c.ReadSeeker.Read(p)
	c.bytes, c.reads = c.bytes+n, c.reads+1
	return n, err
}

// layerProbe reads an MP4's metadata under o and returns its video track's Dolby Vision with what the read cost.
func layerProbe(t *testing.T, path string, o Options) (*mkv.DolbyVision, countedReader) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cr := countedReader{ReadSeeker: bytes.NewReader(b)}
	c, _, err := ReadMeta(context.Background(), &cr, path, o)
	if err != nil || len(c.Tracks) == 0 {
		t.Fatalf("%s: %v", path, err)
	}
	cr.ReadSeeker = nil
	return c.Tracks[0].DolbyVision, cr
}

// TestDolbyVisionLayerMP4: Options.DolbyVisionLayer names the enhancement layer of a profile 7 MP4 from the end of its
// first sample, and a file with no profile 7 track reads nothing more under it.
func TestDolbyVisionLayerMP4(t *testing.T) {
	ctx := context.Background()
	for want, full := range map[mkv.DolbyVisionEL]bool{mkv.DolbyVisionMEL: false, mkv.DolbyVisionFEL: true} {
		dir := t.TempDir()
		fx := dovitest.Profile7(t, dir, full)
		for name, o := range map[string]Options{"flat": {FastStart: true}, "fragmented": {NoIFrames: true}} {
			p7 := filepath.Join(dir, name+"-p7.mp4")
			if err := writeLayout(ctx, fx.Path, p7, o); err != nil {
				t.Fatal(err)
			}
			plain, head := layerProbe(t, p7, Options{})
			if plain == nil || plain.Profile != 7 || plain.EnhancementLayer != mkv.DolbyVisionELUnknown {
				t.Fatalf("%s %s: plain probe gives %+v", name, want, plain)
			}
			got, cost := layerProbe(t, p7, Options{DolbyVisionLayer: true})
			if got.EnhancementLayer != want {
				t.Errorf("%s: enhancement layer %q, want %q", name, got.EnhancementLayer, want)
			}
			if name == "flat" && (cost.bytes <= head.bytes || cost.bytes > head.bytes+8<<10) {
				t.Errorf("%s: the layer cost %d bytes over the %d of the plain probe, the bound is 8 KiB", want, cost.bytes-head.bytes, head.bytes)
			}
			o.DolbyVision = mkv.DolbyVisionToProfile81
			p8 := filepath.Join(dir, name+"-p8.mp4")
			if err := writeLayout(ctx, fx.Path, p8, o); err != nil {
				t.Fatal(err)
			}
			_, without := layerProbe(t, p8, Options{})
			dv, with := layerProbe(t, p8, Options{DolbyVisionLayer: true})
			if with != without || dv == nil || dv.Profile != 8 || dv.EnhancementLayer != mkv.DolbyVisionELUnknown {
				t.Errorf("%s profile 8.1: %+v, %d bytes in %d reads under the option against %d in %d", name, dv, with.bytes, with.reads, without.bytes, without.reads)
			}
		}
	}
}

// writeLayout remuxes src to one MP4 file: flat, or its HLS init and segments end to end when o asks for no fast start.
func writeLayout(ctx context.Context, src, dst string, o Options) error {
	if o.FastStart {
		return RemuxToMP4(ctx, src, dst, o)
	}
	dir := dst + ".hls"
	if err := RemuxToHLS(ctx, src, dir, o); err != nil {
		return err
	}
	parts, err := filepath.Glob(filepath.Join(dir, "seg*.m4s"))
	if err != nil || len(parts) == 0 {
		return errors.Join(err, errors.New("no segment written"))
	}
	sort.Strings(parts)
	var all []byte
	for _, p := range append([]string{filepath.Join(dir, "init.mp4")}, parts...) {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		all = append(all, b...)
	}
	return os.WriteFile(dst, all, 0o644)
}

// TestDolbyVisionOptionsProbeThenConvert: one Options value serves the probe and the conversion. The functions that
// describe a file describe the source as stored - profile 7, its layer, its sample sizes - and the remux converts it.
func TestDolbyVisionOptionsProbeThenConvert(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	fx := dovitest.Profile7(t, dir, true)
	src, dst := filepath.Join(dir, "p7.mp4"), filepath.Join(dir, "p81.mkv")
	if err := RemuxToMP4(ctx, fx.Path, src); err != nil {
		t.Fatal(err)
	}
	o := Options{DolbyVision: mkv.DolbyVisionToProfile81, DolbyVisionLayer: true}
	c, _, err := OpenMeta(ctx, src, o)
	if err != nil {
		t.Fatal(err)
	}
	if dv := c.Tracks[0].DolbyVision; dv == nil || dv.Profile != 7 || !dv.ELPresent || dv.EnhancementLayer != mkv.DolbyVisionFEL {
		t.Errorf("OpenMeta under a conversion describes %+v, the source is profile 7 with a full enhancement layer", dv)
	}
	if _, err := Diagnose(ctx, src, o); err != nil {
		t.Errorf("Diagnose: %v", err)
	}
	if _, _, err := DeclaredSize(ctx, src, o); err != nil {
		t.Errorf("DeclaredSize: %v", err)
	}
	if _, err := VerifyContentHashes(ctx, src, o); errors.Is(err, ErrDolbyVisionUnsupported) {
		t.Errorf("VerifyContentHashes: %v", err)
	}
	var sizes []int64
	if _, _, err := ForEachSample(ctx, src, func(_ *mkv.Track, s Sample) error { sizes = append(sizes, s.Size); return nil }, o); err != nil {
		t.Fatalf("ForEachSample: %v", err)
	}
	file, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _, err := ReadMeta(ctx, bytes.NewReader(file), src, o); err != nil || stored.Tracks[0].DolbyVision.Profile != 7 {
		t.Errorf("ReadMeta: %v", err)
	}
	if len(sizes) != len(fx.Profile81) || sizes[0] == int64(len(fx.Profile81[0])) {
		t.Errorf("ForEachSample gives %d samples, the first of %d bytes: want the %d stored ones, not the converted %d bytes", len(sizes), sizes[0], len(fx.Profile81), len(fx.Profile81[0]))
	}
	if err := RemuxFromMP4(ctx, src, dst, o); err != nil {
		t.Fatal(err)
	}
	out, _ := readMKV(t, dst)
	checkDV(t, "the same Options, converting", o.DolbyVision, videoDV(out.Tracks))
}
