package mp4

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// TestRemuxRefusesDamageInsideTheFile: a remux tolerates a truncated tail by
// stopping at it. An element that overruns the file in the MIDDLE of it read
// the same - and the remux of a 37 s file wrote its first second and reported
// success. It is refused, with the repair named, and leaves no output; the
// same file cut at its tail still remuxes what it holds.
func TestRemuxRefusesDamageInsideTheFile(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
	}
	var blocks []genBlock
	for i := 0; i < 75; i++ { // 3 s at 25 fps: three clusters
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0, data: bytes.Repeat([]byte{0xD0, byte(i)}, 16)})
	}
	src := buildMKV(t, tracks, blocks)
	sound, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()

	// Plant a 2 GiB element over the first block of the second cluster.
	cluster := []byte{0x1F, 0x43, 0xB6, 0x75}
	first := bytes.Index(sound, cluster)
	second := first + 4 + bytes.Index(sound[first+4:], cluster)
	block := second + bytes.IndexByte(sound[second:], 0xA3)
	damaged := append([]byte(nil), sound...)
	copy(damaged[block:], []byte{0xEC, 0x01, 0x00, 0x00, 0x00, 0x7F, 0xFF, 0xFF, 0xFF})
	bad := filepath.Join(dir, "damaged.mkv")
	if err := os.WriteFile(bad, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "damaged.mp4")
	err = RemuxToMP4(ctx, bad, out)
	if !errors.Is(err, reader.ErrDamagedRegion) {
		t.Fatalf("RemuxToMP4 on a file damaged in its second cluster = %v, want the damage refused", err)
	}
	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Errorf("the refused remux left an output behind (stat: %v)", serr)
	}

	// The tolerance this must not break: the same file cut inside its last
	// block is a truncated tail, and still remuxes.
	cut := filepath.Join(dir, "cut.mkv")
	lastCluster := bytes.LastIndex(sound, cluster)
	if err := os.WriteFile(cut, sound[:lastCluster+60], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemuxToMP4(ctx, cut, filepath.Join(dir, "cut.mp4")); err != nil {
		t.Fatalf("RemuxToMP4 on a truncated tail: %v", err)
	}
}
