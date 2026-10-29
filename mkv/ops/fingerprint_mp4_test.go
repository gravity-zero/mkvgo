package ops

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestFingerprint_MP4AsItsMatroskaRemux: an MP4 fingerprints as the Matroska
// its remux writes, subtitles included.
func TestFingerprint_MP4AsItsMatroskaRemux(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	subs := filepath.Join(dir, "subs.mp4")
	if err := mp4.RemuxToMP4(ctx, filepath.Join("..", "..", "internal", "testdata", "sample_subs.mkv"), subs); err != nil {
		t.Fatal(err)
	}
	srcs := []string{subs}
	for _, name := range []string{"bframes_aac.mp4", "fps30.mp4", "hevc_opengop.mp4", "negative_cts.mp4", "interleaved.mp4"} {
		srcs = append(srcs, filepath.Join("..", "..", "internal", "testdata", name))
	}
	for _, src := range srcs {
		remux := filepath.Join(t.TempDir(), "remux.mkv")
		if err := mp4.RemuxFromMP4(ctx, src, remux); err != nil {
			t.Fatal(err)
		}
		got, err := Fingerprint(ctx, src)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(src), err)
		}
		want, err := Fingerprint(ctx, remux)
		if err != nil {
			t.Fatal(err)
		}
		if got.Presentation != want.Presentation || len(got.Tracks) != len(want.Tracks) {
			t.Errorf("%s: %+v\nits remux: %+v", filepath.Base(src), got, want)
			continue
		}
		for i := range got.Tracks {
			if got.Tracks[i] != want.Tracks[i] {
				t.Errorf("%s track %d: %+v, its remux %+v", filepath.Base(src), i, got.Tracks[i], want.Tracks[i])
			}
		}
	}
}

// TestFingerprint_MP4ReadsInBoundedPieces: an MP4 is hashed from the source
// itself - no temporary file, no read of the whole file - so memory stays
// bounded by a read window, as for a Matroska.
func TestFingerprint_MP4ReadsInBoundedPieces(t *testing.T) {
	ctx := context.Background()
	src := largeMP4(t)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} { // os.TempDir reads TMPDIR on Unix, TMP then TEMP on Windows
		t.Setenv(v, tmp)
	}

	var largest int
	fs := &mkv.FS{
		Open: func(p string) (mkv.ReadSeekCloser, error) {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			return &largestRead{File: f, largest: &largest}, nil
		},
		Stat: os.Stat,
	}
	if _, err := Fingerprint(ctx, src, mkv.Options{FS: fs}); err != nil {
		t.Fatal(err)
	}
	if limit := 2 << 20; largest > limit || st.Size() < 4*int64(limit) {
		t.Errorf("largest read %d bytes of a %d-byte source, want at most %d", largest, st.Size(), limit)
	}
}

// largestRead records the largest single read made through it.
type largestRead struct {
	*os.File
	largest *int
}

func (r *largestRead) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	*r.largest = max(*r.largest, n)
	return n, err
}

func (r *largestRead) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.File.ReadAt(p, off)
	*r.largest = max(*r.largest, n)
	return n, err
}

var _ io.ReaderAt = (*largestRead)(nil)

// largeMP4 writes a ~10 MB audio-only MP4 (12 s of 20 kB AAC frames).
func largeMP4(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mkvPath := filepath.Join(dir, "large.mkv")
	f, err := os.Create(mkvPath)
	if err != nil {
		t.Fatal(err)
	}
	sr, ch := 48000.0, uint8(2)
	m := writer.NewMKVWriter(f)
	if err := m.WriteStart(); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000}},
		[]mkv.Track{{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: []byte{0x11, 0x90}, SampleRate: &sr, Channels: &ch}}, 12000); err != nil {
		t.Fatal(err)
	}
	for c := int64(0); c < 12; c++ {
		var blocks []mkv.Block
		for i := int64(0); i < 1000; i += 25 {
			data := make([]byte, 20000)
			data[0], data[1] = byte(c), byte(i)
			blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: c*1000 + i, Keyframe: true, Data: data})
		}
		if err := m.WriteClusterWithCues(c*1000, 1_000_000, blocks); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Finalize(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	dst := filepath.Join(dir, "large.mp4")
	if err := mp4.RemuxToMP4(context.Background(), mkvPath, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}
