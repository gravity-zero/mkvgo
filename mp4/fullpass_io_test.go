package mp4

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// ioTally counts the bytes a full pass reads and writes through Options.FS, and those it reads back from under out.
type ioTally struct {
	read, written int64
	out           string
	readBack      int64
}

type ioTallyReader struct {
	f    mkv.ReadSeekCloser
	t    *ioTally
	back bool
}

func (r *ioTallyReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	r.t.read += int64(n)
	if r.back {
		r.t.readBack += int64(n)
	}
	return n, err
}
func (r *ioTallyReader) Seek(off int64, whence int) (int64, error) { return r.f.Seek(off, whence) }
func (r *ioTallyReader) Close() error                              { return r.f.Close() }

type ioTallyWriter struct {
	f mkv.WriteSeekCloser
	t *ioTally
}

func (w *ioTallyWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.t.written += int64(n)
	return n, err
}
func (w *ioTallyWriter) Seek(off int64, whence int) (int64, error) { return w.f.Seek(off, whence) }
func (w *ioTallyWriter) Close() error                              { return w.f.Close() }

func tallyFS(t *ioTally) *mkv.FS {
	return &mkv.FS{
		Open: func(path string) (mkv.ReadSeekCloser, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			return &ioTallyReader{f: f, t: t, back: t.out != "" && strings.HasPrefix(path, t.out)}, nil
		},
		Create: func(path string) (mkv.WriteSeekCloser, error) {
			f, err := os.Create(path)
			if err != nil {
				return nil, err
			}
			return &ioTallyWriter{f: f, t: t}, nil
		},
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			t.written += int64(len(data))
			return os.WriteFile(path, data, perm)
		},
	}
}

func dirBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	var n int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	return n
}

// TestRemuxToHLSReadsAndWritesOnce: a full pass reads the source once and
// writes each output byte once - no copy of the media through a temp file, no I-frame read back from its segment.
func TestRemuxToHLSReadsAndWritesOnce(t *testing.T) {
	mkvSrc := buildInterleavedSource(t, 60)
	mp4Src := filepath.Join(t.TempDir(), "src.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, mp4Src); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{mkvSrc, mp4Src} {
		t.Run(filepath.Ext(src), func(t *testing.T) {
			st, err := os.Stat(src)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "hls")
			tally := ioTally{out: out}
			if err := RemuxToHLS(context.Background(), src, out, Options{FS: tallyFS(&tally), SourceAccess: SourceLocal}); err != nil {
				t.Fatal(err)
			}
			written := dirBytes(t, out)
			readRatio := float64(tally.read) / float64(st.Size())
			writeRatio := float64(tally.written) / float64(written)
			t.Logf("source %d, read %d (%.3fx), output %d, written %d (%.3fx)", st.Size(), tally.read, readRatio, written, tally.written, writeRatio)
			if readRatio > 1.1 {
				t.Errorf("read %.3fx the source, want at most 1.1x", readRatio)
			}
			if writeRatio > 1.01 {
				t.Errorf("wrote %.3fx the output, want at most 1.01x", writeRatio)
			}
			if tally.readBack != 0 {
				t.Errorf("read back %d bytes of the output", tally.readBack)
			}
			if m, _ := filepath.Glob(filepath.Join(out, "iframe*.m4s")); len(m) == 0 {
				t.Error("no I-frame fragment written")
			}
		})
	}
}

// TestFastStartReadsOnce: the sizing pass of a fast start walks the block
// headers; the source's payloads are read once, by the write pass.
func TestFastStartReadsOnce(t *testing.T) {
	src := buildInterleavedSource(t, 60)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	var tally ioTally
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst, Options{FS: tallyFS(&tally), FastStart: true, SourceAccess: SourceLocal}); err != nil {
		t.Fatal(err)
	}
	out, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	readRatio := float64(tally.read) / float64(st.Size())
	writeRatio := float64(tally.written) / float64(out.Size())
	t.Logf("source %d, read %d (%.3fx), output %d, written %d (%.3fx)", st.Size(), tally.read, readRatio, out.Size(), tally.written, writeRatio)
	if readRatio > 1.1 {
		t.Errorf("read %.3fx the source, want at most 1.1x", readRatio)
	}
	if writeRatio > 1.01 {
		t.Errorf("wrote %.3fx the output, want at most 1.01x", writeRatio)
	}
}

// TestRemuxFromMP4ReadsInRuns: an MP4's samples are stored in runs, read as
// runs - not one read call per sample.
func TestRemuxFromMP4ReadsInRuns(t *testing.T) {
	mkvSrc := buildInterleavedSource(t, 60)
	src := filepath.Join(t.TempDir(), "src.mp4")
	if err := RemuxToMP4(context.Background(), mkvSrc, src); err != nil {
		t.Fatal(err)
	}
	var tally readTally
	dst := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), src, dst, Options{FS: countingFS(&tally)}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("source %d: %d reads, %d bytes", st.Size(), tally.reads, tally.bytes)
	if limit := int(st.Size()/(256<<10)) + 64; tally.reads > limit {
		t.Errorf("%d read calls for a %d-byte source, want at most %d", tally.reads, st.Size(), limit)
	}
	if ratio := float64(tally.bytes) / float64(st.Size()); ratio > 1.05 {
		t.Errorf("read %.3fx the source, want at most 1.05x", ratio)
	}
}
