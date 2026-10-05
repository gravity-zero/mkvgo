package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// bigVideoMKV is an indexed MKV whose video frames are 24 KiB each - a window
// of it is megabytes, like a real one - beside small audio frames.
func bigVideoMKV(t *testing.T) string {
	t.Helper()
	w, h := uint32(1920), uint32(1080)
	sr, ch := 48000.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 250; i++ {
		frame := bytes.Repeat([]byte{byte(i), byte(i >> 8), 0x5A}, 8<<10)
		copy(frame, []byte{0x00, 0x00, 0x00, 0x01, 0x65})
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%50 == 0, data: frame})
	}
	for i := 0; i < 500; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: bytes.Repeat([]byte{0xAA, byte(i)}, 200)})
	}
	sortGenBlocks(gblocks)
	return buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, gblocks)
}

// A window's video is read once, into the buffer that is served: a segment
// costs about its own size in memory, not twice it (one buffer per block,
// then every block copied into the segment).
func TestMatroskaVideoSegmentIsBuiltInPlace(t *testing.T) {
	src := bigVideoMKV(t)
	ctx := context.Background()
	plan, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if plan.NumSegments() < 4 {
		t.Fatalf("%d segments", plan.NumSegments())
	}
	for _, name := range []string{"seg00002.m4s", "seg00003.m4s"} {
		runtime.GC()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		seg, _, err := plan.Resource(ctx, name)
		runtime.ReadMemStats(&m1)
		if err != nil {
			t.Fatal(err)
		}
		if len(seg) < 1<<20 {
			t.Fatalf("%s is %d bytes: the fixture no longer builds a megabyte window", name, len(seg))
		}
		if ratio := float64(m1.TotalAlloc-m0.TotalAlloc) / float64(len(seg)); ratio > 1.5 {
			t.Errorf("%s: %.2f times its %d bytes allocated to build it, want about once", name, ratio, len(seg))
		}
	}

	// And the bytes are the full pass's.
	dir := t.TempDir()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	fresh, err := PlanHLS(ctx, src, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range fresh.Resources() {
		got, _, err := fresh.Resource(ctx, name)
		if err != nil {
			t.Errorf("Resource(%q): %v", name, err)
			continue
		}
		want, ferr := os.ReadFile(filepath.Join(dir, name))
		if ferr != nil || name == "master.m3u8" || name == "manifest.mpd" {
			continue // only the plan serves it, or it carries the plan's bandwidth estimate
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the full pass (%d vs %d bytes)", name, len(got), len(want))
		}
	}
}
