package ops

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDemuxAllocatesNoFrameCopies: a demux allocated each block's payload and, for H.264, its Annex-B copy again -
// twice the stream's bytes, a collection cycle every few MB. Buffers reused frame to frame leave the bytes allocated
// a fraction of the stream's.
func TestDemuxAllocatesNoFrameCopies(t *testing.T) {
	dir := t.TempDir()
	sps, pps := []byte{0x67, 0x64, 0x00, 0x1F}, []byte{0x68, 0xEE}
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: avcRecord(sps, pps), Width: u32(64), Height: u32(64)}
	const frames, size = 200, 64 << 10
	src := demuxFixture(t, dir, []mkv.Track{video}, frames, func(_ uint64, i int) ([]byte, bool) {
		nal := make([]byte, size)
		nal[0] = 0x41
		return append([]byte{0, 1, 0, 0}, nal...), i%10 == 0
	})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: filepath.Join(dir, "out")}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if got, stream := after.TotalAlloc-before.TotalAlloc, uint64(frames*size); got > stream/2 {
		t.Errorf("demux of %d bytes of frames allocated %d bytes, want under half", stream, got)
	}
}
