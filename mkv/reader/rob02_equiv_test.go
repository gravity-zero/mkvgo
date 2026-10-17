package reader

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// The pre-ROB-02 resync, kept to check the in-window one finds the same element.
func old_resyncToCluster(r io.ReadSeeker, scanLimit, fitLimit int64) (int64, error) {
	from, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	for {
		off, err := old_scanForClusterMagic(r, from, scanLimit)
		if err != nil || off < 0 {
			return -1, err
		}
		valid, err := old_isClusterAt(r, off, fitLimit)
		if err != nil {
			return -1, err
		}
		if valid {
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				return -1, err
			}
			return off, nil
		}
		from = off + 1 // false positive: resume scanning just past it
	}
}
func old_scanForClusterMagic(r io.ReadSeeker, from, limit int64) (int64, error) {
	return old_scanForMagic(r, from, limit, func(b []byte) int { return bytes.Index(b, clusterMagic) })
}
func old_scanForMagic(r io.ReadSeeker, from, limit int64, index func([]byte) int) (int64, error) {
	if _, err := r.Seek(from, io.SeekStart); err != nil {
		return -1, err
	}
	const window = 64 << 10
	buf := make([]byte, len(clusterMagic)-1+window)
	tail := 0
	next := from
	for {
		base := next - int64(tail) // absolute offset of buf[0]
		n, rerr := r.Read(buf[tail : tail+window])
		end := tail + n

		search := buf[:end]
		if limit >= 0 {
			if max := limit - base; max < int64(end) {
				if max < 0 {
					max = 0
				}
				search = buf[:max]
			}
		}
		if i := index(search); i >= 0 {
			return base + int64(i), nil
		}

		next += int64(n)
		if limit >= 0 && next >= limit {
			return -1, nil
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return -1, nil
			}
			return -1, rerr
		}
		if n == 0 {
			return -1, nil
		}
		keep := len(clusterMagic) - 1
		if end < keep {
			keep = end
		}
		copy(buf[:keep], buf[end-keep:end])
		tail = keep
	}
}
func old_isClusterAt(r io.ReadSeeker, off, limit int64) (bool, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	h, _, err := ebml.ReadElementHeader(r)
	if err != nil || h.ID != mkv.IDCluster {
		return false, nil
	}
	bodyStart, _ := r.Seek(0, io.SeekCurrent)
	if h.Size >= 0 && limit >= 0 && bodyStart+h.Size > limit {
		return false, nil // declared size overruns the segment - not a real cluster
	}
	child, _, err := ebml.ReadElementHeader(r)
	if err != nil {
		return false, nil
	}
	return clusterChildIDs[child.ID], nil
}

func old_resyncToSegmentElement(r io.ReadSeeker, scanLimit, fitLimit int64) (int64, error) {
	from, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	if fitLimit < 0 {
		if fitLimit, err = r.Seek(0, io.SeekEnd); err != nil {
			return -1, err
		}
	}
	if scanLimit < 0 || scanLimit > fitLimit {
		scanLimit = fitLimit
	}
	for {
		off, err := old_scanForMagic(r, from, scanLimit, indexSegmentLevelID)
		if err != nil || off < 0 {
			return -1, err
		}
		valid, err := old_isSegmentElementAt(r, off, fitLimit)
		if err != nil {
			return -1, err
		}
		if valid {
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				return -1, err
			}
			return off, nil
		}
		from = off + 1 // false positive: resume scanning just past it
	}
}

func old_isSegmentElementAt(r io.ReadSeeker, off, limit int64) (bool, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	var buf [segmentElementProbe]byte
	n, err := io.ReadFull(r, buf[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	h, hn, ok := segmentElementPrefix(buf[:n])
	if !ok {
		return false, nil
	}
	return h.Size < 0 || off+int64(hn)+h.Size <= limit, nil
}

func TestROB02_ResyncEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	found := 0
	pieces := [][]byte{
		{0x1F, 0x43, 0xB6, 0x75, 0x00},
		{0x1F, 0x43, 0xB6, 0x75, 0x83, 0xE7, 0x81, 0x00},
		{0x1F, 0x43, 0xB6, 0x75, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xA3},
		{0x1F, 0x43, 0xB6, 0x75, 0x88, 0x42},
		{0x15, 0x49, 0xA9, 0x66, 0x84, 0x2A, 0xD7, 0xB1, 0x80},
		{0x16, 0x54, 0xAE, 0x6B, 0x82, 0xAE, 0x80},
		{0x1C, 0x53, 0xBB, 0x6B, 0x82, 0xEC, 0x80},
	}
	for n := 0; n < 300; n++ {
		size := 1 + rng.Intn(3*64<<10)
		data := make([]byte, size)
		rng.Read(data)
		for k := rng.Intn(40); k > 0; k-- {
			p := pieces[rng.Intn(len(pieces))]
			at := rng.Intn(size)
			if rng.Intn(3) == 0 { // near a 64 KiB read boundary
				at = (1+rng.Intn(2))*(64<<10) + rng.Intn(80) - 40
			}
			if at >= 0 && at < size {
				copy(data[at:], p)
			}
		}
		from := int64(rng.Intn(size))
		for _, limit := range []int64{-1, int64(size), int64(rng.Intn(size + 1)), from + int64(rng.Intn(70<<10))} {
			check := func(name string, cur, old func(io.ReadSeeker) (int64, error)) {
				a, b := bytes.NewReader(data), bytes.NewReader(data)
				_, _ = a.Seek(from, io.SeekStart)
				_, _ = b.Seek(from, io.SeekStart)
				got, gerr := cur(a)
				want, werr := old(b)
				if got != want || (gerr == nil) != (werr == nil) {
					t.Fatalf("%s case %d from %d limit %d: got (%d,%v), was (%d,%v)", name, n, from, limit, got, gerr, want, werr)
				}
				if got >= 0 {
					found++
					if pos, _ := a.Seek(0, io.SeekCurrent); pos != got {
						t.Fatalf("%s: left at %d, found %d", name, pos, got)
					}
				}
			}
			fit := limit
			if rng.Intn(2) == 0 {
				fit = -1
			}
			check("cluster", func(r io.ReadSeeker) (int64, error) { return resyncToCluster(r, limit, fit) },
				func(r io.ReadSeeker) (int64, error) { return old_resyncToCluster(r, limit, fit) })
			segFit := int64(size)
			check("segment", func(r io.ReadSeeker) (int64, error) { return resyncToSegmentElement(r, limit, segFit) },
				func(r io.ReadSeeker) (int64, error) { return old_resyncToSegmentElement(r, limit, segFit) })
		}
	}
	if found < 300 {
		t.Fatalf("only %d resyncs found an element", found)
	}
}
