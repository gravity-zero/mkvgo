package reader_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/ops"
	"github.com/gravity-zero/mkvgo/mkv/reader"
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

// probeCost reads path head-only under opts and returns its video track with what the read cost.
func probeCost(t *testing.T, path string, opts ...reader.ReadOption) (mkv.Track, countedReader) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cr := countedReader{ReadSeeker: bytes.NewReader(b)}
	c, err := reader.ReadMeta(context.Background(), &cr, path, opts...)
	if err != nil || len(c.Tracks) == 0 {
		t.Fatalf("%s: %v", path, err)
	}
	cr.ReadSeeker = nil
	return c.Tracks[0], cr
}

// TestDolbyVisionLayer: the probe said "profile 7" and nothing of what the enhancement layer holds, which decides
// whether dropping it loses picture detail. WithDolbyVisionLayer reads it from the first sample's RPU.
func TestDolbyVisionLayer(t *testing.T) {
	for want, full := range map[mkv.DolbyVisionEL]bool{mkv.DolbyVisionMEL: false, mkv.DolbyVisionFEL: true} {
		fx := dovitest.Profile7(t, t.TempDir(), full)
		plain, head := probeCost(t, fx.Path)
		if plain.DolbyVision == nil || plain.DolbyVision.Profile != 7 || plain.DolbyVision.EnhancementLayer != mkv.DolbyVisionELUnknown {
			t.Fatalf("%s: head-only read gives %+v", want, plain.DolbyVision)
		}
		got, cost := probeCost(t, fx.Path, reader.WithDolbyVisionLayer())
		if got.DolbyVision.EnhancementLayer != want {
			t.Errorf("enhancement layer %q, want %q", got.DolbyVision.EnhancementLayer, want)
		}
		if cost.bytes <= head.bytes {
			t.Errorf("%s: the layer was read with no sample read (%d bytes, head %d)", want, cost.bytes, head.bytes)
		}
		both, shared := probeCost(t, fx.Path, reader.WithDolbyVisionLayer(), reader.WithInBandColourFallback())
		_, colour := probeCost(t, fx.Path, reader.WithInBandColourFallback())
		if both.DolbyVision.EnhancementLayer != want || (colour.bytes > head.bytes && shared.bytes != colour.bytes) {
			t.Errorf("%s with the colour fallback: layer %q, %d bytes read against %d for the colour alone", want, both.DolbyVision.EnhancementLayer, shared.bytes, colour.bytes)
		}
	}
}

// TestDolbyVisionLayerUnknown: a first sample whose RPU does not read leaves the layer unstated, with no error.
func TestDolbyVisionLayerUnknown(t *testing.T) {
	fx := dovitest.Profile7(t, t.TempDir(), true)
	b, err := os.ReadFile(fx.Path)
	if err != nil {
		t.Fatal(err)
	}
	b[bytes.Index(b, []byte{0x7C, 0x01, 0x19})+12] ^= 0xFF // the first sample's RPU
	if err := os.WriteFile(fx.Path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := probeCost(t, fx.Path, reader.WithDolbyVisionLayer()); got.DolbyVision.EnhancementLayer != mkv.DolbyVisionELUnknown {
		t.Errorf("damaged RPU read as %q", got.DolbyVision.EnhancementLayer)
	}
}

// TestDolbyVisionLayerCostsOnlyProfile7: a file with no profile 7 track - profile 8.1, or no Dolby Vision at all -
// reads not one byte more under the option.
func TestDolbyVisionLayerCostsOnlyProfile7(t *testing.T) {
	dir := t.TempDir()
	fx := dovitest.Profile7(t, dir, true)
	for name, conv := range map[string]mkv.DolbyVisionConversion{"profile 8.1": mkv.DolbyVisionToProfile81, "no Dolby Vision": mkv.DolbyVisionToBaseLayer} {
		path := filepath.Join(dir, name+".mkv")
		if err := ops.ConvertDolbyVision(context.Background(), fx.Path, path, conv); err != nil {
			t.Fatal(err)
		}
		_, without := probeCost(t, path)
		got, with := probeCost(t, path, reader.WithDolbyVisionLayer())
		if with != without {
			t.Errorf("%s: %d bytes in %d reads under the option, %d in %d without", name, with.bytes, with.reads, without.bytes, without.reads)
		}
		if got.DolbyVision != nil && got.DolbyVision.EnhancementLayer != mkv.DolbyVisionELUnknown {
			t.Errorf("%s: layer %q on a track that has none", name, got.DolbyVision.EnhancementLayer)
		}
	}
}

// TestDolbyVisionLayerBigFirstBlock: the first block of a UHD film runs to megabytes and the option read all of it.
// The read is the block's last 8 KiB whatever its size, and an RPU further than that from the end is left unstated
// instead of being looked for.
func TestDolbyVisionLayerBigFirstBlock(t *testing.T) {
	const big = 6 << 20
	var extra [2]int
	for i, pad := range []int{0, big} {
		fx := dovitest.Build(t, t.TempDir(), dovitest.Spec{Full: true, Pad: pad})
		_, head := probeCost(t, fx.Path)
		got, cost := probeCost(t, fx.Path, reader.WithDolbyVisionLayer())
		if got.DolbyVision.EnhancementLayer != mkv.DolbyVisionFEL {
			t.Errorf("first block padded by %d bytes: layer %q, want FEL", pad, got.DolbyVision.EnhancementLayer)
		}
		extra[i] = cost.bytes - head.bytes
		t.Logf("first block padded by %d bytes: %d bytes read beyond the head-only probe, in %d reads", pad, extra[i], cost.reads-head.reads)
	}
	if extra[1] > extra[0]+64<<10 || extra[1] > 256<<10 {
		t.Errorf("a first block %d bytes larger cost %d bytes more (%d against %d)", big, extra[1]-extra[0], extra[1], extra[0])
	}
	far := dovitest.Build(t, t.TempDir(), dovitest.Spec{Full: true, Pad: big, Trail: 16 << 10})
	_, head := probeCost(t, far.Path)
	got, cost := probeCost(t, far.Path, reader.WithDolbyVisionLayer())
	if got.DolbyVision.EnhancementLayer != mkv.DolbyVisionELUnknown || cost.bytes-head.bytes > extra[1]+64<<10 {
		t.Errorf("RPU 16 KiB from the end of the block: layer %q for %d bytes read, want it unstated within the same budget", got.DolbyVision.EnhancementLayer, cost.bytes-head.bytes)
	}
	near := dovitest.Build(t, t.TempDir(), dovitest.Spec{Full: true, Pad: big, Trail: 1 << 10})
	if got, _ := probeCost(t, near.Path, reader.WithDolbyVisionLayer()); got.DolbyVision.EnhancementLayer != mkv.DolbyVisionFEL {
		t.Errorf("RPU 1 KiB from the end of the block: layer %q, want FEL", got.DolbyVision.EnhancementLayer)
	}
}
