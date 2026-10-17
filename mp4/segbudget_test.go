package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSegmentBytesBounded: a segment whose samples the source declares at hundreds of MiB (overlapping, each within
// the file's claimed length) is refused before its buffer is allocated.
func TestSegmentBytesBounded(t *testing.T) {
	const n, size = 3, 100 << 20
	mono := stereo48k
	build := func(at uint32) []byte {
		stts := fullBox("stts", 0, 0, func(w *bw) { w.u32(1); w.u32(n); w.u32(48000) })
		stsc := fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(1); w.u32(1) })
		stsz := fullBox("stsz", 0, 0, func(w *bw) {
			w.u32(0)
			w.u32(n)
			for range n {
				w.u32(size)
			}
		})
		stco := fullBox("stco", 0, 0, func(w *bw) {
			w.u32(n)
			for range n {
				w.u32(at) // every sample over the same bytes
			}
		})
		moov := container("moov", mvhdBox(n*1000),
			soundTrak(1, 48000, n*48000, audioSampleEntry("mp4a", &mono, esdsBox(0x40, fakeASC)), stts, stsc, stsz, stco))
		return append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), moov...)
	}
	head := build(0)
	head = build(uint32(len(head)))
	path := filepath.Join(t.TempDir(), "big.mp4")
	if err := os.WriteFile(path, head, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(len(head))+size); err != nil { // sparse: holds every sample's bytes
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := PlanHLS(ctx, path, Options{SegmentMs: 10000})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range plan.Resources() {
		if filepath.Ext(name) != ".m4s" {
			continue
		}
		if b, _, err := plan.Resource(ctx, name); err == nil {
			t.Errorf("%s: a %d-byte segment was built", name, len(b))
		}
	}
}
