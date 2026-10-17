package reader

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
)

// zeroedFixture is the live fixture with its second cluster zeroed from 40
// bytes in to 40 bytes before the third: the pieces a download never received.
func zeroedFixture(t *testing.T) (data []byte, from, third int64) {
	t.Helper()
	data = append([]byte(nil), livefixture.Build(livefixture.Options{})...)
	first := bytes.Index(data, clusterMagic)
	second := first + 4 + bytes.Index(data[first+4:], clusterMagic)
	third = int64(second + 4 + bytes.Index(data[second+4:], clusterMagic))
	from = int64(second + 40)
	for i := from; i < third-40; i++ {
		data[i] = 0
	}
	return data, from, third
}

// TestNextClassifiesAnUnreadableElementAsDamage: a header that does not decode
// in the middle of the file, with a valid Cluster behind it, is damage - the
// same DamageError as an element that overruns the file - and a walk asked to
// skip it resumes on that Cluster and names the region. A bounded look that
// finds no Cluster reports the damage without a Resume; the source's own
// failure is never taken for damage; a seat inside the zeros stays the decode
// error it is unless the walk bounded its look.
func TestNextClassifiesAnUnreadableElementAsDamage(t *testing.T) {
	data, from, third := zeroedFixture(t)
	seat := livefixture.SegmentBodyOffset(livefixture.Options{})
	walk := func(br *BlockReader) (blocks int, err error) {
		for {
			if _, err = br.Next(); err != nil {
				return blocks, err
			}
			blocks++
		}
	}
	open := func(at int64) *BlockReader {
		t.Helper()
		br, err := NewBlockReaderAt(bytes.NewReader(data), 1_000_000, at)
		if err != nil {
			t.Fatal(err)
		}
		return br
	}

	var de *DamageError
	br := open(seat)
	before, err := walk(br)
	if !errors.As(err, &de) || de.Resume != third || de.Offset > from+64 || de.Offset < from-64 { // the element that failed: the one holding the first zeros, or the next
		t.Fatalf("unbounded walk: %d blocks, err %v; want a DamageError resuming at %d from about %d", before, err, third, from)
	}
	if !errors.Is(err, ErrDamagedRegion) {
		t.Errorf("the DamageError does not match ErrDamagedRegion")
	}

	br = open(seat)
	br.SetDamageScan(16) // shorter than the zeros
	if _, err := walk(br); !errors.As(err, &de) || de.Resume != -1 {
		t.Errorf("bounded look too short: err %v; want a DamageError without Resume", err)
	}

	br = open(seat)
	br.SetSkipDamage(true)
	br.SetDamageScan(int64(len(data)))
	after, err := walk(br)
	if !errors.Is(err, io.EOF) || after <= before {
		t.Errorf("skipping walk: %d blocks (%d before the damage), err %v; want the blocks of the third cluster and a clean end", after, before, err)
	}
	if rs := br.SkippedRanges(); len(rs) != 1 || rs[0].To != third || rs[0].From > from+64 || rs[0].From < from-64 || br.SkippedBytes() != rs[0].To-rs[0].From {
		t.Errorf("skipped ranges %v (%d bytes); want one ending at %d", rs, br.SkippedBytes(), third)
	}

	br = open(from + 8) // seated inside the zeros: a stale position, unless the look is bounded
	if _, err := walk(br); err == nil || errors.Is(err, ErrDamagedRegion) {
		t.Errorf("seat in the zeros, unbounded: err %v; want the decode error itself", err)
	}
	br = open(from + 8)
	br.SetDamageScan(int64(len(data)))
	if _, err := walk(br); !errors.As(err, &de) || de.Resume != third {
		t.Errorf("seat in the zeros, bounded look: err %v; want a DamageError resuming at %d", err, third)
	}

	boom := errors.New("boom")
	br, err = NewBlockReaderAt(&failingReader{Reader: bytes.NewReader(data), after: from + 8, err: boom}, 1_000_000, seat)
	if err != nil {
		t.Fatal(err)
	}
	br.SetSkipDamage(true)
	br.SetDamageScan(int64(len(data)))
	if _, err := walk(br); !errors.Is(err, boom) || errors.Is(err, ErrDamagedRegion) {
		t.Errorf("source failing: err %v; want its own error, not damage", err)
	}
}

// failingReader returns err on any read reaching past `after`.
type failingReader struct {
	*bytes.Reader
	after int64
	err   error
}

func (f *failingReader) Read(p []byte) (int, error) {
	pos, _ := f.Seek(0, io.SeekCurrent)
	if pos+int64(len(p)) > f.after {
		return 0, f.err
	}
	return f.Reader.Read(p)
}
