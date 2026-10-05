package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// zeroTailMKV builds a small indexed MKV and appends n null bytes to it, the
// way some muxers close a file: a few 0x00 after the last element.
func zeroTailMKV(t *testing.T, n int) (path string, blocks int) {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 150; i++ {
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 300; i++ {
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true, data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	src := buildMKV(t, []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
	}, gblocks)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "tail.mkv")
	if err := os.WriteFile(path, append(data, make([]byte, n)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, len(gblocks)
}

// walkBlocks walks the file from its first Cluster with a reader seated
// there, as the on-demand paths seat theirs: such a reader does not know where
// the Segment ends, so it meets whatever follows the last element.
func walkBlocks(t *testing.T, path string) (int, error) {
	t.Helper()
	c, err := reader.OpenMeta(context.Background(), path, reader.WithCues())
	if err != nil || len(c.Cues) == 0 {
		t.Fatalf("fixture metadata: %v (%d cues)", err, len(c.Cues))
	}
	return walkBlocksFrom(t, path, c.Info.TimecodeScale, c.SegmentStart+c.Cues[0].ClusterPos)
}

func walkBlocksFrom(t *testing.T, path string, timecodeScale, off int64) (int, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	br, err := reader.NewBlockReaderAt(f, timecodeScale, off)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		if _, err := br.Next(); err != nil {
			return n, err
		}
		n++
	}
}

// A few null bytes after the last element are how some muxers end a file, and
// every player reads such a file to its end. The block walk must end cleanly
// on them, and the plan must be the one the same file gives without them.
func TestNullBytesAfterTheLastElementEndTheFile(t *testing.T) {
	clean, want := zeroTailMKV(t, 0)
	ref, err := PlanHLS(context.Background(), clean, Options{SegmentMs: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2, 4, 8, 41, 68, maxZeroTailForTest} {
		path, _ := zeroTailMKV(t, n)
		got, err := walkBlocks(t, path)
		if !errors.Is(err, io.EOF) || got != want {
			t.Errorf("%d null bytes: the walk read %d of %d blocks and ended on %v, want io.EOF", n, got, want, err)
		}
		plan, err := PlanHLS(context.Background(), path, Options{SegmentMs: 2000})
		if err != nil {
			t.Errorf("%d null bytes: PlanHLS: %v", n, err)
			continue
		}
		for _, name := range ref.Resources() {
			a, _, aerr := ref.Resource(context.Background(), name)
			b, _, berr := plan.Resource(context.Background(), name)
			if aerr != nil || berr != nil {
				t.Errorf("%d null bytes: %s: %v, %v", n, name, aerr, berr)
				continue
			}
			if name == "master.m3u8" || name == "manifest.mpd" {
				// The declared bandwidth is estimated from the source's bytes,
				// and the file is n bytes longer: nothing else may move.
				a, b = bandwidthRE.ReplaceAll(a, nil), bandwidthRE.ReplaceAll(b, nil)
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%d null bytes: %s differs from the file without them:\n%s\n---\n%s", n, name, a, b)
			}
		}
	}
}

// A long run of null bytes at the end is not a muxer's closing padding: it is
// a file whose tail was never written, and the walk keeps saying so.
func TestALongNullTailIsStillAnError(t *testing.T) {
	path, _ := zeroTailMKV(t, maxZeroTailForTest+1)
	if _, err := walkBlocks(t, path); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("a %d-byte null tail ended the walk on %v, want an error", maxZeroTailForTest+1, err)
	}
}

// nullBytesInLastCluster rewrites the fixture so that its last Cluster is the
// last thing in the file and declares n more bytes than it holds, those bytes
// being null: the Cluster says it carries blocks there.
func nullBytesInLastCluster(t *testing.T, n int) (walk func() (int, error), blocks int) {
	t.Helper()
	src, blocks := zeroTailMKV(t, 0)
	c, err := reader.OpenMeta(context.Background(), src, reader.WithCues())
	if err != nil || len(c.Cues) == 0 {
		t.Fatalf("fixture metadata: %v (%d cues)", err, len(c.Cues))
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	at := c.SegmentStart + c.Cues[len(c.Cues)-1].ClusterPos
	h, hdrLen, err := ebml.ReadElementHeader(bytes.NewReader(data[at:]))
	if err != nil || h.ID != 0x1F43B675 {
		t.Fatalf("no Cluster at the last cue position: id %X, %v", h.ID, err)
	}
	end := at + int64(hdrLen) + h.Size
	width := hdrLen - 4 // the size field follows the 4-byte Cluster ID
	size := uint64(h.Size) + uint64(n)
	if width < 2 || size >= 1<<(7*uint(width))-1 {
		t.Fatalf("the Cluster's %d-byte size field cannot hold %d", width, size)
	}
	for i := width - 1; i >= 0; i-- {
		data[at+4+int64(i)] = byte(size)
		size >>= 8
	}
	data[at+4] |= 1 << (8 - uint(width))
	path := filepath.Join(t.TempDir(), "incluster.mkv")
	if err := os.WriteFile(path, append(data[:end:end], make([]byte, n)...), 0o644); err != nil {
		t.Fatal(err)
	}
	first := c.SegmentStart + c.Cues[0].ClusterPos
	return func() (int, error) { return walkBlocksFrom(t, path, c.Info.TimecodeScale, first) }, blocks
}

// Null bytes INSIDE a Cluster of declared size stand in for blocks the
// Cluster says it holds. A handful is still a muxer's padding; more is media
// that is not there, whatever the allowance after the last element.
func TestNullBytesInsideTheLastCluster(t *testing.T) {
	walk, want := nullBytesInLastCluster(t, 8)
	if got, err := walk(); !errors.Is(err, io.EOF) || got != want {
		t.Errorf("8 null bytes in the Cluster: %d of %d blocks, ended on %v, want io.EOF", got, want, err)
	}
	walk, _ = nullBytesInLastCluster(t, 200)
	if _, err := walk(); err == nil || errors.Is(err, io.EOF) {
		t.Errorf("200 null bytes in the Cluster ended the walk on %v, want an error", err)
	}
}

const maxZeroTailForTest = 4096

var bandwidthRE = regexp.MustCompile(`(?i)bandwidth="?[0-9]+"?`)
