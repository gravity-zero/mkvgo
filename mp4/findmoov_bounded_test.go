package mp4

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// sparseFile is a seekable file of zeros of a declared size with a few byte
// ranges written into it, counting what is read: a multi-hundred-megabyte
// source without a byte of it in memory.
type sparseFile struct {
	size    int64
	patches map[int64][]byte
	pos     int64
	read    int64
}

func (f *sparseFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.pos = off
	case io.SeekCurrent:
		f.pos += off
	case io.SeekEnd:
		f.pos = f.size + off
	}
	return f.pos, nil
}

func (f *sparseFile) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), f.size-f.pos))
	clear(p[:n])
	for at, b := range f.patches {
		lo, hi := max(at, f.pos), min(at+int64(len(b)), f.pos+int64(n))
		if lo < hi {
			copy(p[lo-f.pos:hi-f.pos], b[lo-at:hi-at])
		}
	}
	f.pos += int64(n)
	f.read += int64(n)
	return n, nil
}

// brokenMP4 is an ISO-BMFF file of the given size whose first box after ftyp
// declares a size that runs past the end (the forward walk desyncs on it), with
// a moov box - mvhd child included - written at each of moovAt.
func brokenMP4(size int64, moovAt ...int64) *sparseFile {
	head := make([]byte, 32)
	binary.BigEndian.PutUint32(head[0:], 16)
	copy(head[4:], "ftypisom")
	binary.BigEndian.PutUint32(head[16:], 0x7FFFFFF0)
	copy(head[20:], "mdat")
	f := &sparseFile{size: size, patches: map[int64][]byte{0: head}}
	for _, at := range moovAt {
		moov := make([]byte, 16)
		binary.BigEndian.PutUint32(moov[0:], 116) // moov: header + a 108-byte mvhd
		copy(moov[4:], "moov")
		binary.BigEndian.PutUint32(moov[8:], 108)
		copy(moov[12:], "mvhd")
		f.patches[at] = moov
	}
	return f
}

func allocatedBy(fn func()) uint64 {
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	fn()
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc
}

// The backward scan for a moov must find it wherever it sits in the scanned
// tail - across the boundary between two chunks of the scan in particular -
// and must do so in bounded memory: it read the whole window into one buffer,
// up to 256 MiB of it for every file it was pointed at.
func TestFindMoovBackwardScansInBoundedMemory(t *testing.T) {
	const size = 300 << 20
	for _, back := range []int64{200, moovScanChunk - 3, moovScanChunk, moovScanChunk + 1, moovScanChunk + 4,
		moovScanChunk + 7, 2*moovScanChunk + 2, 100<<20 + 5, maxMoovScanWindow - 8} {
		at := size - back
		f := brokenMP4(size, at)
		var off, plen int64
		var err error
		alloc := allocatedBy(func() { off, plen, err = findMoov(f, size) })
		if err != nil {
			t.Errorf("moov %d bytes before the end: %v", back, err)
			continue
		}
		if off != at+8 || plen != 108 {
			t.Errorf("moov %d bytes before the end: found payload at %d (%d bytes), want %d (108)", back, off, plen, at+8)
		}
		if alloc > 8<<20 {
			t.Errorf("moov %d bytes before the end: the scan allocated %d MiB", back, alloc>>20)
		}
	}

	// A "moov" in the media data that is not a box is passed over for the real
	// one before it.
	f := brokenMP4(size, size-(50<<20))
	f.patches[size-4096] = []byte{0, 0, 0, 1, 'm', 'o', 'o', 'v', 0xFF, 0xFF, 0xFF, 0xFF}
	if off, _, err := findMoov(f, size); err != nil || off != size-(50<<20)+8 {
		t.Errorf("decoy after the moov: payload at %d, err %v", off, err)
	}

	// No moov at all: the same refusal as before, having read the tail once
	// and held one chunk of it.
	f = brokenMP4(size)
	var err error
	alloc := allocatedBy(func() { _, _, err = findMoov(f, size) })
	if err == nil || err.Error() != "mp4: no moov box found" {
		t.Errorf("file without a moov: err = %v", err)
	}
	if alloc > 8<<20 {
		t.Errorf("file without a moov: the scan allocated %d MiB", alloc>>20)
	}
	if f.read > maxMoovScanWindow+(4<<20) {
		t.Errorf("file without a moov: %d MiB read, want the %d MiB tail once", f.read>>20, maxMoovScanWindow>>20)
	}
}

// A file that does not open on an ISO base media box is not an MP4: it is
// refused on its first bytes, not after a scan of its last 256 MiB for a moov
// it cannot hold (an MPEG-TS carrying an .mp4 name, here).
func TestNonISOBMFFSourceIsRefusedOnItsHead(t *testing.T) {
	const size = 1250 << 20
	f := &sparseFile{size: size, patches: map[int64][]byte{0: {0x47, 0x40, 0x00, 0x10, 0x00, 0x00, 0xB0, 0x11}}}
	_, _, err := findMoov(f, size)
	if !errors.Is(err, ErrNotMP4) {
		t.Fatalf("findMoov on an MPEG-TS head: err = %v, want ErrNotMP4", err)
	}
	if f.read > 64 {
		t.Errorf("%d bytes read to refuse a file on its head", f.read)
	}

	path := filepath.Join(t.TempDir(), "ts.mp4")
	ts := make([]byte, 1<<20)
	for i := 0; i < len(ts); i += 188 {
		ts[i] = 0x47
	}
	if err := os.WriteFile(path, ts, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanHLS(context.Background(), path); !errors.Is(err, ErrNotMP4) {
		t.Errorf("PlanHLS: err = %v, want ErrNotMP4", err)
	}
	if _, _, err := OpenMeta(context.Background(), path); !errors.Is(err, ErrNotMP4) {
		t.Errorf("OpenMeta: err = %v, want ErrNotMP4", err)
	}
	if err := RemuxToHLS(context.Background(), path, t.TempDir()); !errors.Is(err, ErrNotMP4) {
		t.Errorf("RemuxToHLS: err = %v, want ErrNotMP4", err)
	}
}

// Metadata reads look for the moov once: the lazy read falling back to the
// full one must not scan a file without a moov a second time.
func TestMetaReadLooksForTheMoovOnce(t *testing.T) {
	const size = 300 << 20
	f := brokenMP4(size)
	if _, err := readMoovForMode(f, size, sampleNone); err == nil {
		t.Fatal("a file without a moov was read")
	}
	if f.read > maxMoovScanWindow+(4<<20) {
		t.Errorf("%d MiB read: the tail was scanned more than once", f.read>>20)
	}
}
