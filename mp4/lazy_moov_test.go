package mp4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// largeMoovMP4 builds an MP4 whose moov exceeds the lazy reader's chunk (large
// stsz/stco from many samples), so the lazy path makes several reads and seeks
// over the skipped bodies — the case the small fixture cannot exercise.
func largeMoovMP4(t *testing.T) string {
	t.Helper()
	const n = 30000
	w, h := uint32(1920), uint32(1080)
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "V_MPEGH/ISO/HEVC", CodecPrivate: bareHvcC(), Width: &w, Height: &h}}
	data := []byte{0, 0, 0, 1, 0x40}
	blks := make([]genBlock, n)
	for i := range blks {
		blks[i] = genBlock{track: 1, pts: int64(i * 40), key: i%24 == 0, data: data}
	}
	mkvPath := buildMKV(t, tracks, blks)
	dst := filepath.Join(t.TempDir(), "large.mp4")
	if err := RemuxToMP4(context.Background(), mkvPath, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

func parseFile(t *testing.T, data []byte, mode sampleMode) *movie {
	t.Helper()
	mv, err := parseMP4(bytes.NewReader(data), int64(len(data)), mode)
	if err != nil {
		t.Fatalf("parseMP4(%d): %v", mode, err)
	}
	return mv
}

func assertLazyFullParity(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	lazyBuf, err := readMoovLazy(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("readMoovLazy: %v", err)
	}
	fullBuf, err := readMoov(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("readMoov: %v", err)
	}
	// Parse both buffers in the SAME mode so only the moov read differs.
	lazyMv, err := parseMoov(lazyBuf, size, sampleKeyframes)
	if err != nil {
		t.Fatalf("parseMoov(lazy): %v", err)
	}
	fullMv, err := parseMoov(fullBuf, size, sampleKeyframes)
	if err != nil {
		t.Fatalf("parseMoov(full): %v", err)
	}
	lc, fc := containerFromMovie(lazyMv), containerFromMovie(fullMv)
	if !reflect.DeepEqual(lc.Tracks, fc.Tracks) {
		t.Errorf("%s tracks differ:\n lazy = %+v\n full = %+v", path, lc.Tracks, fc.Tracks)
	}
	if lc.DurationMs != fc.DurationMs {
		t.Errorf("%s duration: lazy=%d full=%d", path, lc.DurationMs, fc.DurationMs)
	}
	if !reflect.DeepEqual(lc.Keyframes, fc.Keyframes) {
		t.Errorf("%s keyframes differ (lazy %d vs full %d)", path, len(lc.Keyframes), len(fc.Keyframes))
	}
}

// TestLazyMoovMetaParity proves the lazy moov read yields a Container identical
// to a full read — on a small metadata-rich file and on a large file where the
// big sample-table bodies are actually skipped.
func TestLazyMoovMetaParity(t *testing.T) {
	assertLazyFullParity(t, buildTestMP4(t))
	assertLazyFullParity(t, largeMoovMP4(t))
}

// failSeekBeyond is a ReadSeeker that errors on any SeekStart past limit. The
// lazy reader seeks per chunk deep into the moov and so hits it; the full read
// only seeks to the moov start and then reads sequentially, staying under limit.
type failSeekBeyond struct {
	rs    io.ReadSeeker
	limit int64
}

func (f *failSeekBeyond) Read(p []byte) (int, error) { return f.rs.Read(p) }
func (f *failSeekBeyond) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekStart && off > f.limit {
		return 0, errors.New("seek beyond limit")
	}
	return f.rs.Seek(off, whence)
}

// TestLazyMoovFallback proves the mandatory safety net: when the lazy read cannot
// complete (a reader that refuses the deep per-chunk seeks), parseMP4 falls back
// to the full read and parses correctly — never an error, same metadata.
func TestLazyMoovFallback(t *testing.T) {
	data, err := os.ReadFile(largeMoovMP4(t))
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	dataOff, _, err := findMoov(bytes.NewReader(data), size)
	if err != nil {
		t.Fatal(err)
	}

	// The lazy read alone must fail under this reader (else the fallback is untested).
	if _, err := readMoovLazy(&failSeekBeyond{rs: bytes.NewReader(data), limit: dataOff}, size); err == nil {
		t.Fatal("expected readMoovLazy to fail under the seek-limited reader")
	}

	mv, err := parseMP4(&failSeekBeyond{rs: bytes.NewReader(data), limit: dataOff}, size, sampleKeyframes)
	if err != nil {
		t.Fatalf("fallback parse: %v", err)
	}
	viaFallback := containerFromMovie(mv)
	want := containerFromMovie(parseFile(t, data, sampleKeyframes))
	if !reflect.DeepEqual(viaFallback.Tracks, want.Tracks) || !reflect.DeepEqual(viaFallback.Keyframes, want.Keyframes) {
		t.Error("fallback produced different metadata than the normal lazy read")
	}
}
