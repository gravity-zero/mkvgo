package mp4

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/poison"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestRemuxToMP4ReusesTheBlockBuffer: the samples are read into one buffer the
// walk reuses, since each is copied into its track's chunk at once; a buffer per
// block allocated the whole source once per pass (3.8 GB for a 1.8 GB fast start).
func TestRemuxToMP4ReusesTheBlockBuffer(t *testing.T) {
	if poison.On {
		t.Skip("poisoning allocates a fresh buffer at each reuse")
	}
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: []byte{1, 2, 3, 4},
		Width: u32p(64), Height: u32p(64), FrameRate: f64p(25)}}
	var blocks []genBlock
	for i := int64(0); i < 200; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: i * 40, key: i%25 == 0, data: make([]byte, 64<<10)})
	}
	src := buildMKV(t, tracks, blocks)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out.mp4")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := RemuxToMP4(context.Background(), src, dst, Options{FastStart: true}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if alloc := int64(after.TotalAlloc - before.TotalAlloc); alloc > st.Size()/2 {
		t.Errorf("a fast-start remux of a %d-byte source allocated %d bytes, want <= %d", st.Size(), alloc, st.Size()/2)
	}
}

// TestVerifyContentHashesStreamsEachSample pins that a sample is hashed as it is read, not held whole: an 8 MiB
// sample was allocated whole, a forged table's 4 GiB one too.
func TestVerifyContentHashesStreamsEachSample(t *testing.T) {
	if poison.On {
		t.Skip("poisoning allocates a fresh buffer at each reuse")
	}
	const big = 8 << 20
	data := append([]byte{0, 0, 0, 1, 0x65}, make([]byte, big)...)
	w, h := uint32(320), uint32(240)
	src := buildMKV(t, []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h}},
		[]genBlock{{track: 1, pts: 0, key: true, data: data}})
	ctx := context.Background()
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(ctx, src, out, Options{ContentHashes: true}); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	mismatches, err := VerifyContentHashes(ctx, out)
	runtime.ReadMemStats(&after)
	if err != nil || len(mismatches) != 0 {
		t.Fatalf("verify: %v, %v", mismatches, err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > big/4 {
		t.Errorf("verify allocated %d bytes for an %d-byte sample", alloc, big)
	}
}
