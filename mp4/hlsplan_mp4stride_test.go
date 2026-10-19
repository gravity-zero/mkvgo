package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// opusTiedLeadMP4 is an Opus MP4 whose first three samples share a time and whose stride lies off the millisecond grid: a lace-like start nothing confirms.
func opusTiedLeadMP4(t *testing.T, n int) []byte {
	t.Helper()
	head := []byte{'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 2, 0, 0, 0x80, 0xBB, 0, 0, 0, 0, 0}
	dops, err := dOpsBox(head)
	if err != nil {
		t.Fatal(err)
	}
	tr := mkv.Track{Codec: "opus", Channels: u8p(2), SampleRate: f64p(48000)}
	const size = 8
	payload := make([]byte, n*size)
	for i := range payload {
		payload[i] = byte(i)
		if i%size == 0 {
			payload[i] = 0xFC
		}
	}
	total := uint32(100 + (n-3)*1000)
	build := func(off uint32) []byte {
		stts := fullBox("stts", 0, 0, func(w *bw) {
			w.u32(3)
			w.u32(2)
			w.u32(0)
			w.u32(1)
			w.u32(100)
			w.u32(uint32(n - 3))
			w.u32(1000)
		})
		stsc := fullBox("stsc", 0, 0, func(w *bw) { w.u32(1); w.u32(1); w.u32(uint32(n)); w.u32(1) })
		stsz := fullBox("stsz", 0, 0, func(w *bw) { w.u32(size); w.u32(uint32(n)) })
		stco := fullBox("stco", 0, 0, func(w *bw) { w.u32(1); w.u32(off) })
		moov := container("moov", mvhdBox(total/48),
			soundTrak(1, 48000, total, audioSampleEntry("Opus", &tr, dops), stts, stsc, stsz, stco))
		return append(box("ftyp", []byte("isom\x00\x00\x02\x00isom")), moov...)
	}
	h := build(0)
	h = build(uint32(len(h)) + 8)
	return append(h, box("mdat", payload)...)
}

// The plan of an MP4 source settles an unconfirmed measured stride as the full pass does, so both serve the same bytes.
func TestPlanHLSFromMP4SettlesAnUnconfirmedStride(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "opus.mp4")
	if err := os.WriteFile(src, opusTiedLeadMP4(t, 500), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out := filepath.Join(dir, "hls")
	if err := RemuxToHLS(ctx, src, out, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range plan.Resources() {
		got, _, err := plan.Resource(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("the full pass did not write %s", name)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs between the plan and the full pass", name)
		}
	}
}
