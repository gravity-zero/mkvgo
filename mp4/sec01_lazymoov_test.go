package mp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/httpfs"
	"github.com/gravity-zero/mkvgo/internal/costguard"
)

// declaredSource is a source that claims size bytes but holds only head: past it, reads find EOF.
type declaredSource struct {
	head []byte
	size int64
	pos  int64
}

func (s *declaredSource) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.head)) {
		return 0, io.EOF
	}
	n := copy(p, s.head[s.pos:])
	s.pos += int64(n)
	return n, nil
}

func (s *declaredSource) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = off
	case io.SeekCurrent:
		s.pos += off
	case io.SeekEnd:
		s.pos = s.size + off
	}
	return s.pos, nil
}

// hugeMoovHeader is a 16-byte moov box header whose largesize spans size bytes.
func hugeMoovHeader(size int64) []byte {
	h := make([]byte, 16)
	binary.BigEndian.PutUint32(h[0:4], 1)
	copy(h[4:8], "moov")
	binary.BigEndian.PutUint64(h[8:16], uint64(size))
	return h
}

func totalAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

const sec01Declared = int64(1) << 40

func TestSEC01_LazyMoovCap(t *testing.T) {
	src := &declaredSource{head: hugeMoovHeader(sec01Declared), size: sec01Declared}
	before := totalAlloc()
	_, _, err := ReadMeta(context.Background(), src, "huge.mp4")
	grew := totalAlloc() - before
	if err == nil {
		t.Fatal("a moov declared over the source's 16 real bytes was read")
	}
	if grew >= 300<<20 {
		t.Fatalf("ReadMeta allocated %d MiB for a 16-byte source declaring %d bytes", grew>>20, sec01Declared)
	}
}

func TestSEC01_LazyMoovCapHTTP(t *testing.T) {
	head := hugeMoovHeader(sec01Declared)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil || a >= int64(len(head)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, len(head)-1, sec01Declared))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(head[a:])
	}))
	defer srv.Close()
	before := totalAlloc()
	_, _, err := OpenMetaWithFS(context.Background(), srv.URL+"/huge.mp4", httpfs.New().Port())
	grew := totalAlloc() - before
	if err == nil {
		t.Fatal("a moov declared over the server's 16 real bytes was read")
	}
	if grew >= 300<<20 {
		t.Fatalf("OpenMeta over HTTP allocated %d MiB for a 16-byte object declaring %d bytes", grew>>20, sec01Declared)
	}
}

// FuzzReadMetaDeclaredSize: whatever size a source declares past the bytes it holds, ReadMeta allocates in proportion to the bytes, not the claim.
func FuzzReadMetaDeclaredSize(f *testing.F) {
	f.Add(hugeMoovHeader(1<<40), int64(1)<<40)
	f.Add(hugeMoovHeader(300<<20), int64(300)<<20)
	f.Add(append([]byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0}, hugeMoovHeader(1<<50)...), int64(1)<<50)
	f.Fuzz(func(t *testing.T, head []byte, declared int64) {
		costguard.Fuzz(t, len(head), func() {
			if declared < int64(len(head)) {
				declared = int64(len(head))
			}
			src := &declaredSource{head: head, size: declared}
			before := totalAlloc()
			_, _, _ = ReadMeta(context.Background(), src, "fuzz.mp4")
			if grew := totalAlloc() - before; grew > 300<<20+64*uint64(len(head)) {
				t.Fatalf("ReadMeta allocated %d MiB for %d real bytes declaring %d", grew>>20, len(head), declared)
			}
		})
	})
}

// moovOfZeros is a source whose moov declares n bytes and delivers them, all zero, as a server streaming them or a sparse file would.
func moovOfZeros(n int64) *zeroTail {
	return &zeroTail{head: hugeMoovHeader(16 + n), size: 16 + n}
}

func TestSEC01_MoovHardCap(t *testing.T) {
	const declared = 300 << 20
	before := totalAlloc()
	_, _, err := ReadMeta(context.Background(), moovOfZeros(declared), "zeros.mp4")
	grew := totalAlloc() - before
	if err == nil || !strings.Contains(err.Error(), "MaxMoovBytes") {
		t.Fatalf("a %d MiB moov past the cap: err %v, want a refusal naming Options.MaxMoovBytes", declared>>20, err)
	}
	if grew > 16<<20 {
		t.Fatalf("refusing a %d MiB moov allocated %d MiB: it was read", declared>>20, grew>>20)
	}
	small := moovOfZeros(1 << 20)
	if _, _, err := ReadMeta(context.Background(), small, "zeros.mp4", Options{MaxMoovBytes: 512 << 10}); err == nil || !strings.Contains(err.Error(), "MaxMoovBytes") {
		t.Fatalf("a 1 MiB moov under a 512 KiB MaxMoovBytes: %v", err)
	}
	if _, _, err := ReadMeta(context.Background(), moovOfZeros(1<<20), "zeros.mp4", Options{MaxMoovBytes: 2 << 20}); err != nil && strings.Contains(err.Error(), "MaxMoovBytes") {
		t.Fatalf("a 1 MiB moov under a 2 MiB MaxMoovBytes is refused by the cap: %v", err)
	}
}

func TestSEC01_MoovHardCapHTTP(t *testing.T) {
	const declared = int64(300) << 20
	head := hugeMoovHeader(16 + declared)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		b = min(b, 16+declared-1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, 16+declared))
		w.WriteHeader(http.StatusPartialContent)
		body := make([]byte, b-a+1)
		if a < int64(len(head)) {
			copy(body, head[a:])
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	before := totalAlloc()
	_, _, err := OpenMetaWithFS(context.Background(), srv.URL+"/zeros.mp4", httpfs.New().Port())
	if grew := totalAlloc() - before; err == nil || grew > 16<<20 {
		t.Fatalf("a server streaming a %d MiB moov: err %v, %d MiB allocated", declared>>20, err, grew>>20)
	}
}
