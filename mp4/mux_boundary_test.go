package mp4

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// mux_boundary_test.go targets mux.go survivors: the chunk-flush thresholds
// (emitSample), the Progress/DoStat wiring in RemuxToMP4, the dst.Close()
// error propagation, the chapter-track threshold in planTracks and the
// hasSamples check in streamSamples.

// TestEmitSampleFlushesOnByteThreshold kills mux.go:97 (CONDITIONALS_BOUNDARY
// and CONDITIONALS_NEGATION on `len(t.pending) >= chunkByteThreshold`): one
// byte short of the threshold must not flush; reaching it exactly must.
func TestEmitSampleFlushesOnByteThreshold(t *testing.T) {
	newTrack := func() *outTrack { return &outTrack{mkv: mkv.Track{ID: 1}} }
	cw := &countWriter{w: io.Discard}

	t.Run("below-threshold-no-flush", func(t *testing.T) {
		tr := newTrack()
		data := make([]byte, chunkByteThreshold-1)
		if err := tr.emitSample(cw, data, 0, 0, 0, true); err != nil {
			t.Fatal(err)
		}
		if len(tr.samples.chunks) != 0 {
			t.Errorf("chunkByteThreshold-1 bytes must not flush a chunk, got %d chunks", len(tr.samples.chunks))
		}
		if tr.pendingCnt != 1 {
			t.Errorf("pendingCnt = %d, want 1 (sample still buffered)", tr.pendingCnt)
		}
	})

	t.Run("at-threshold-flushes", func(t *testing.T) {
		tr := newTrack()
		data := make([]byte, chunkByteThreshold)
		if err := tr.emitSample(cw, data, 0, 0, 0, true); err != nil {
			t.Fatal(err)
		}
		if len(tr.samples.chunks) != 1 {
			t.Fatalf("chunkByteThreshold bytes must flush exactly one chunk, got %d", len(tr.samples.chunks))
		}
		if tr.samples.chunks[0].count != 1 {
			t.Errorf("flushed chunk count = %d, want 1", tr.samples.chunks[0].count)
		}
		if tr.pendingCnt != 0 || len(tr.pending) != 0 {
			t.Errorf("pending state must reset after a flush: pendingCnt=%d pending=%d", tr.pendingCnt, len(tr.pending))
		}
	})
}

// TestPlanTracksChapterThreshold kills mux.go:280 (CONDITIONALS_BOUNDARY on
// `len(chs) > 0`): chapters that all flatten away (untitled) must not
// synthesize a chapter track; a single titled chapter must.
func TestPlanTracksChapterThreshold(t *testing.T) {
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240)}

	t.Run("all-untitled-no-chapter-track", func(t *testing.T) {
		c := &mkv.Container{Tracks: []mkv.Track{video}, Chapters: []mkv.Chapter{{StartMs: 0, Title: ""}}}
		tracks, _, err := planTracks(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(tracks) != 1 {
			t.Errorf("got %d tracks, want 1 (no synthesized chapter track for zero flattened chapters)", len(tracks))
		}
	})

	t.Run("one-titled-chapter-adds-track", func(t *testing.T) {
		c := &mkv.Container{Tracks: []mkv.Track{video}, Chapters: []mkv.Chapter{{StartMs: 0, Title: "Chapter 1"}}}
		tracks, _, err := planTracks(c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(tracks) != 2 {
			t.Fatalf("got %d tracks, want 2 (video + synthesized chapter track)", len(tracks))
		}
		if !tracks[1].isChapter {
			t.Error("second track must be the synthesized chapter track")
		}
	})
}

// TestRemuxToMP4NoSamplesInAnyTrack kills mux.go:460 (CONDITIONALS_BOUNDARY on
// `len(t.samples.samples) > 0`): a track declared but carrying zero blocks
// must still trip the "no samples found in any track" error.
func TestRemuxToMP4NoSamplesInAnyTrack(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	src := buildMKV(t, tracks, nil)
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), src, dst); err == nil {
		t.Fatal("expected \"no samples found in any track\" error")
	}
}

// TestRemuxToMP4ProgressAndStatWiring kills mux.go:149 (NEGATION on
// `o.Progress != nil`), mux.go:150 (INVERT_NEGATIVES/ARITHMETIC_BASE on the
// int64(-1) sentinel) and mux.go:151 (NEGATION on `e == nil`): the sentinel
// -1 must reach Progress when DoStat fails, and the real size when it
// succeeds.
func TestRemuxToMP4ProgressAndStatWiring(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	// The reader reports progress every 50 blocks (progressInterval); enough
	// blocks must be generated for at least one report to fire.
	var blocks []genBlock
	for i := 0; i < 120; i++ {
		blocks = append(blocks, genBlock{track: 1, pts: int64(i) * 40, key: i == 0, data: []byte{1, 2, 3, byte(i)}})
	}
	src := buildMKV(t, tracks, blocks)

	t.Run("stat-fails-total-is-negative-one", func(t *testing.T) {
		var totals []int64
		fs := &mkv.FS{Stat: func(path string) (os.FileInfo, error) {
			return nil, errors.New("injected stat failure")
		}}
		dst := filepath.Join(t.TempDir(), "out.mp4")
		err := RemuxToMP4(context.Background(), src, dst, Options{FS: fs, Progress: func(processed, total int64) {
			totals = append(totals, total)
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(totals) == 0 {
			t.Fatal("Progress was never called")
		}
		if totals[0] != -1 {
			t.Errorf("total on the first Progress call = %d, want -1 (DoStat failed, size unknown)", totals[0])
		}
	})

	t.Run("stat-succeeds-total-is-real-size", func(t *testing.T) {
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		wantSize := fi.Size()
		var totals []int64
		dst := filepath.Join(t.TempDir(), "out.mp4")
		if err := RemuxToMP4(context.Background(), src, dst, Options{Progress: func(processed, total int64) {
			totals = append(totals, total)
		}}); err != nil {
			t.Fatal(err)
		}
		if len(totals) == 0 {
			t.Fatal("Progress was never called")
		}
		for _, tot := range totals {
			if tot != wantSize {
				t.Errorf("total = %d, want the real source size %d", tot, wantSize)
			}
		}
	})

	t.Run("no-progress-callback-not-invoked", func(t *testing.T) {
		// Sanity: nothing must panic and the remux must still succeed when
		// Options.Progress is nil (the o.Progress != nil branch must be
		// correctly skipped, not entered on a nil func).
		dst := filepath.Join(t.TempDir(), "out.mp4")
		if err := RemuxToMP4(context.Background(), src, dst); err != nil {
			t.Fatal(err)
		}
	})
}

// closeErrWriteSeeker wraps a real *os.File, forcing Close() to fail while
// leaving Write/Seek delegated normally, to test dst.Close() error handling.
type closeErrWriteSeeker struct {
	*os.File
	closeErr error
}

func (c *closeErrWriteSeeker) Close() error {
	_ = c.File.Close()
	return c.closeErr
}

// TestRemuxToMP4ClosePropagatesWhenNoPriorError kills mux.go:162 (NEGATION on
// `err == nil`): a dst.Close() failure must surface as the returned error
// when the remux itself otherwise succeeded.
func TestRemuxToMP4ClosePropagatesWhenNoPriorError(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{{track: 1, pts: 0, key: true, data: []byte{1, 2, 3, 4}}}
	src := buildMKV(t, tracks, blocks)

	dstPath := filepath.Join(t.TempDir(), "out.mp4")
	closeErr := errors.New("injected close failure")
	fs := &mkv.FS{Create: func(path string) (mkv.WriteSeekCloser, error) {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		return &closeErrWriteSeeker{File: f, closeErr: closeErr}, nil
	}}
	err := RemuxToMP4(context.Background(), src, dstPath, Options{FS: fs})
	if err == nil {
		t.Fatal("expected the injected dst.Close() failure to propagate")
	}
}
