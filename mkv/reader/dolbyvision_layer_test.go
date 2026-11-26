package reader_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/internal/dovi"
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

// remoteCost is what one cold head-only read over HTTP asked of the server.
type remoteCost struct {
	gets  []string
	bytes int64
}

// remoteProbe serves path over HTTP, a strong ETag on its responses when etag is set, and reads it head-only under
// opts through a new httpfs.FS: the video track, with the ranges asked and the bytes transferred.
func remoteProbe(t *testing.T, path string, etag bool, opts ...reader.ReadOption) (mkv.Track, remoteCost) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var cost remoteCost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cost.gets = append(cost.gets, r.Method+" "+r.Header.Get("Range"))
		mu.Unlock()
		if etag {
			w.Header().Set("ETag", `"v1"`)
		}
		http.ServeContent(w, r, "f.mkv", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	fs := httpfs.New()
	c, err := reader.OpenMetaWithFS(context.Background(), srv.URL+"/f.mkv", fs.Port(), opts...)
	if err != nil || len(c.Tracks) == 0 {
		t.Fatalf("%s: %v", path, err)
	}
	cost.bytes = fs.BytesFetched()
	return c.Tracks[0], cost
}

// TestDolbyVisionLayerRemote: over HTTP the option fetched the head window a second time and a whole window for the
// 8 KiB it reads. The walk reads the head again from what the open already fetched and the tail is asked for exactly:
// nothing more for a file its first window holds, one request of 8 KiB for a large first block, and one window more
// when attachments push the first cluster out of the head.
func TestDolbyVisionLayerRemote(t *testing.T) {
	const window, big = 512 << 10, 6 << 20
	for _, tc := range []struct {
		name                string
		spec                dovitest.Spec
		want                mkv.DolbyVisionEL
		offGets             int // what the read costs without the option, unchanged
		moreGets, moreBytes int // the most the option adds
	}{
		{"file within the first window", dovitest.Spec{Full: true}, mkv.DolbyVisionFEL, 1, 0, 0},
		{"minimal layer", dovitest.Spec{}, mkv.DolbyVisionMEL, 1, 0, 0},
		{"large first block", dovitest.Spec{Full: true, Pad: big}, mkv.DolbyVisionFEL, 2, 1, dovi.TailBytes},
		{"RPU out of the budget", dovitest.Spec{Full: true, Pad: big, Trail: 16 << 10}, mkv.DolbyVisionELUnknown, 2, 1, dovi.TailBytes},
		{"attachments ahead of the first cluster", dovitest.Spec{Full: true, Attach: 3 << 20}, mkv.DolbyVisionFEL, 2, 1, window},
		{"attachments and a large first block", dovitest.Spec{Full: true, Attach: 3 << 20, Pad: big}, mkv.DolbyVisionFEL, 2, 2, window + dovi.TailBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := dovitest.Build(t, t.TempDir(), tc.spec)
			plain, off := remoteProbe(t, fx.Path, true)
			got, on := remoteProbe(t, fx.Path, true, reader.WithDolbyVisionLayer())
			if plain.DolbyVision.EnhancementLayer != mkv.DolbyVisionELUnknown || len(off.gets) != tc.offGets || off.bytes > window+1<<10 {
				t.Errorf("without the option: layer %q, %d bytes in %v, want %d requests within a window", plain.DolbyVision.EnhancementLayer, off.bytes, off.gets, tc.offGets)
			}
			if got.DolbyVision.EnhancementLayer != tc.want {
				t.Errorf("layer %q, want %q", got.DolbyVision.EnhancementLayer, tc.want)
			}
			moreGets, moreBytes := len(on.gets)-len(off.gets), on.bytes-off.bytes
			t.Logf("the option costs %d requests and %d bytes more: %v", moreGets, moreBytes, on.gets[len(off.gets):])
			if moreGets > tc.moreGets || moreBytes > int64(tc.moreBytes) {
				t.Errorf("the option costs %d requests and %d bytes more, want at most %d and %d: %v", moreGets, moreBytes, tc.moreGets, tc.moreBytes, on.gets)
			}
			for _, g := range on.gets {
				if !strings.HasPrefix(g, "GET bytes=") {
					t.Errorf("request %q is no ranged GET", g)
				}
			}
			if weak, _ := remoteProbe(t, fx.Path, false, reader.WithDolbyVisionLayer()); weak.DolbyVision.EnhancementLayer != tc.want {
				t.Errorf("no strong ETag to share reads under: layer %q, want %q", weak.DolbyVision.EnhancementLayer, tc.want)
			}
		})
	}
}
