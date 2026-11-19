package commands

import (
	"image"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gravity-zero/mkvgo/matroska"
)

// TestBitmapCuesSharingAPictureEncodeItOnce: a picture composed again by a few bytes of stream was quantized and
// PNG-encoded anew for every cue. Each cue still gets its file.
func TestBitmapCuesSharingAPictureEncodeItOnce(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	write := func(cues int) (uint64, string) {
		dir := filepath.Join(t.TempDir(), "out")
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		n, err := writePGSDir(dir, func(fn func(matroska.PGSCue) error) error {
			for i := 0; i < cues; i++ {
				if err := fn(matroska.PGSCue{StartMs: int64(i) * 1000, EndMs: int64(i)*1000 + 500, Image: img}); err != nil {
					return err
				}
			}
			return nil
		})
		runtime.ReadMemStats(&after)
		if err != nil || n != cues {
			t.Fatalf("writePGSDir: %d cues, %v", n, err)
		}
		return after.TotalAlloc - before.TotalAlloc, dir
	}
	one, _ := write(1)
	twenty, dir := write(20)
	if twenty > 3*one {
		t.Errorf("20 cues of one picture allocated %d KiB, one cue %d KiB: the picture was encoded more than once", twenty>>10, one>>10)
	}
	first, err1 := os.ReadFile(filepath.Join(dir, "00000.png"))
	last, err2 := os.ReadFile(filepath.Join(dir, "00019.png"))
	if err1 != nil || err2 != nil || len(first) == 0 || string(first) != string(last) {
		t.Errorf("cue files: %v, %v, %d and %d bytes; want every cue's file, the same picture", err1, err2, len(first), len(last))
	}
}
