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
